package gittransport

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

func TestSSHPushEndToEndReceivePack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)

	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(crypto.Signer(clientPriv))
	if err != nil {
		t.Fatal(err)
	}
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(crypto.Signer(hostPriv))
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go serveFakeReceivePack(listener, hostSigner, clientSigner.PublicKey(), remoteDir)

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write("main", "memory.md", []byte("persistent memory\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	newSHA, err := local.Commit("main", "initial memory")
	if err != nil {
		t.Fatal(err)
	}

	block, err := ssh.MarshalPrivateKey(clientPriv, "test")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(block))
	addr := listener.Addr().(*net.TCPAddr)
	url := "ssh://git@127.0.0.1:" + itoa(addr.Port) + "/unused.git"

	result, err := Push(context.Background(), local, url, "main", keyPEM, ssh.FingerprintSHA256(hostSigner.PublicKey()), 10*time.Second)
	if err != nil {
		t.Fatalf("SSH push failed: %v", err)
	}
	if result.New != newSHA {
		t.Fatalf("result SHA %s != %s", result.New, newSHA)
	}

	out, err := exec.Command("git", "--git-dir", remoteDir, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("remote rev-parse: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != newSHA {
		t.Fatalf("remote head=%s want=%s", strings.TrimSpace(string(out)), newSHA)
	}
}

func TestSSHPushRejectsWrongPinnedHostKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(crypto.Signer(hostPriv))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go serveFakeReceivePack(listener, hostSigner, nil, remoteDir)
	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = local.Write("main", "a", []byte("a"), gitstore.ModeContent)
	_, _ = local.Commit("main", "seed")
	block, _ := ssh.MarshalPrivateKey(clientPriv, "test")
	addr := listener.Addr().(*net.TCPAddr)
	url := "ssh://git@127.0.0.1:" + itoa(addr.Port) + "/unused.git"
	_, err = Push(context.Background(), local, url, "main", string(pem.EncodeToMemory(block)), "SHA256:wrong", time.Second)
	if err == nil || !strings.Contains(err.Error(), "host key 不匹配") {
		t.Fatalf("want host-key failure; got %v", err)
	}
}

func serveFakeReceivePack(l net.Listener, hostSigner ssh.Signer, allowed ssh.PublicKey, repoDir string) {
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if allowed == nil || string(key.Marshal()) != string(allowed.Marshal()) {
			return nil, io.ErrUnexpectedEOF
		}
		return &ssh.Permissions{}, nil
	}}
	cfg.AddHostKey(hostSigner)
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			serverConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
			if err != nil {
				conn.Close()
				return
			}
			go ssh.DiscardRequests(reqs)
			go func() {
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						_ = newCh.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					ch, requests, err := newCh.Accept()
					if err != nil {
						continue
					}
					go handleReceivePackChannel(ch, requests, repoDir)
				}
			}()
			_ = serverConn.Wait()
		}(conn)
	}
}

func handleReceivePackChannel(ch ssh.Channel, requests <-chan *ssh.Request, repoDir string) {
	defer ch.Close()
	for req := range requests {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var msg struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &msg); err != nil || !strings.HasPrefix(msg.Command, "git-receive-pack ") {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		cmd := exec.Command("git", "receive-pack", repoDir)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		cmd.Stderr = ch.Stderr()
		if err := cmd.Start(); err != nil {
			return
		}
		copyDone := make(chan struct{})
		go func() { _, _ = io.Copy(stdin, ch); _ = stdin.Close(); close(copyDone) }()
		_, _ = io.Copy(ch, stdout)
		<-copyDone
		waitErr := cmd.Wait()
		status := uint32(0)
		if waitErr != nil {
			status = 1
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

func runTestGit(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
