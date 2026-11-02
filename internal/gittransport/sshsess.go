package gittransport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/zero-memcp/internal/gitremote"
)

// sshSession 是一条已经跑起远端 git 命令的 SSH 会话。
//
// 抽出来是因为 push 和 fetch 的差别只在「执行哪个命令、发什么帧」，
// 拨号、认证、host key 校验、超时、stderr 收集这些必须完全一致——
// 复制两份的话，迟早有一份忘了校验 host key。
type sshSession struct {
	client     *ssh.Client
	session    *ssh.Session
	stdin      io.WriteCloser
	stdout     io.Reader
	stderrDone chan []byte
	path       string
}

func dialSSH(ctx context.Context, remoteURL, privateKeyPEM, hostKey string, timeout time.Duration) (*sshSession, error) {
	if timeout <= 0 {
		timeout = gitremote.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	_ = cancel // 连接建立后由 deadline 接管

	ep, err := gitremote.ParseSSHURL(remoteURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(hostKey, "SHA256:") {
		return nil, errors.New("SSH host_key must be a SHA256:... fingerprint")
	}
	signer, err := ssh.ParsePrivateKey([]byte(privateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("parse SSH private key (only unencrypted OpenSSH keys are supported): %w", err)
	}
	want := strings.TrimSpace(hostKey)
	cfg := &ssh.ClientConfig{
		User: ep.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got := ssh.FingerprintSHA256(key)
			if got != want {
				return fmt.Errorf("SSH host key mismatch: configured %s, remote %s", want, got)
			}
			return nil
		},
		Timeout: timeout,
	}

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", ep.Address())
	if err != nil {
		return nil, fmt.Errorf("connect to SSH %s: %w", ep.Host, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, ep.Address(), cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SSH handshake: %w", err)
	}
	client := ssh.NewClient(cc, chans, reqs)

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("create SSH session: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(stderr, 64<<10))
		done <- b
	}()
	return &sshSession{client: client, session: session, stdin: stdin, stdout: stdout, stderrDone: done, path: ep.Path}, nil
}

// setenv 尝试给远端命令带环境变量（协议 v2 靠 GIT_PROTOCOL 开启）。
//
// 服务端可能拒绝 setenv（sshd 的 AcceptEnv 白名单）。拒绝不是错误：
// 只是意味着这次只能用 v0，所以调用方忽略返回值。
func (s *sshSession) setenv(key, value string) error {
	return s.session.Setenv(key, value)
}

func (s *sshSession) start(gitCommand string) error {
	if err := s.session.Start(gitCommand + " " + shellQuote(s.path)); err != nil {
		return fmt.Errorf("start remote %s: %w", gitCommand, err)
	}
	return nil
}

func (s *sshSession) wait() error {
	if err := s.session.Wait(); err != nil {
		stderr := <-s.stderrDone
		if len(stderr) != 0 {
			return fmt.Errorf("remote command failed: %w: %s", err, strings.TrimSpace(string(stderr)))
		}
		return fmt.Errorf("remote command failed: %w", err)
	}
	select {
	case <-s.stderrDone:
	default:
	}
	return nil
}

func (s *sshSession) close() {
	if s.session != nil {
		s.session.Close()
	}
	if s.client != nil {
		s.client.Close()
	}
}
