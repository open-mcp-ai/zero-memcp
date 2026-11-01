package server

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 最小闭环：root 通过 remote-key 存私钥 → push 走内置 SSH 客户端 →
// 远端 ref 真的前进。不调用系统 git 作为客户端（远端仍是被推的一方）。
func TestSSHPushThroughServerVerbs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remoteDir)

	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(crypto.Signer(clientPriv))
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(crypto.Signer(hostPriv))
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveReceivePackSSH(ln, hostSigner, clientSigner.PublicKey(), remoteDir)

	port := ln.Addr().(*net.TCPAddr).Port
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project  = "daily"
name     = "backup"
url      = "ssh://git@127.0.0.1:%d/unused.git"
push     = ["zero"]
ssh_key  = "deploy"
host_key = %q
`, dataDir, port, ssh.FingerprintSHA256(hostSigner.PublicKey()))
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "note.md", []byte("备忘\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Commit("zero", "seed")
	if err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})
	// 只读凭证不得写入私钥。
	roTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "ro", Cap: "r"})

	block, err := ssh.MarshalPrivateKey(clientPriv, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(block))

	// 私钥只能通过 vars 传入；输出里绝不能出现私钥内容。
	out, err := callMCPWithVars(t, srv, rootTok, "remote-key put deploy -- $KEY", map[string]string{"KEY": keyPEM})
	if err != nil {
		t.Fatalf("remote-key put 失败: %v", err)
	}
	if strings.Contains(out, "PRIVATE KEY") {
		t.Fatalf("私钥内容被回显了: %s", out)
	}
	if _, err := callMCPWithVars(t, srv, roTok, "remote-key put other -- $KEY", map[string]string{"KEY": keyPEM}); err == nil {
		t.Fatal("只读凭证不该能写私钥")
	}

	if _, err := callMCP(t, srv, rootTok, "push daily backup zero"); err != nil {
		t.Fatalf("SSH push 失败: %v", err)
	}

	remoteHead := strings.TrimSpace(gitOutput(t, remoteDir, "rev-parse", "refs/heads/zero"))
	if remoteHead != head {
		t.Fatalf("远端应推进到 %s，实际 %s", head, remoteHead)
	}
}

// 私钥文件必须 0600，且读不到时不静默降级。
func TestRemoteKeyFilePermissions(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPWithVars(t, srv, tok, "remote-key put k1 -- $KEY", map[string]string{"KEY": string(pem.EncodeToMemory(block))}); err != nil {
		t.Fatalf("保存私钥失败: %v", err)
	}
	keyPath := filepath.Join(dataDir, "system", "sshkeys", "k1.pem")
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode=%04o, want 0600", keyInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(keyPath))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("private key dir mode=%04o, want 0700", dirInfo.Mode().Perm())
	}
	st, err := srv.readSSHKey("k1")
	if err != nil || len(st) == 0 {
		t.Fatalf("应能读回私钥: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "remote-key delete k1"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := srv.readSSHKey("k1"); err == nil {
		t.Fatal("删除后不该还能读到")
	}
}

// callMCPWithVars 把正文放进 vars，验证「数据与指令分离」在 push 路径上也成立。
func callMCPWithVars(t *testing.T, srv *Server, token, cmd string, vars map[string]string) (string, error) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "term",
			"arguments": map[string]any{"cmd": cmd, "vars": vars},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		return "", errors.New(strings.TrimSpace(rec.Body.String()))
	}
	return mcpText(t, rec.Body.Bytes())
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func serveReceivePackSSH(l net.Listener, hostSigner ssh.Signer, allowed ssh.PublicKey, repoDir string) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if allowed == nil || string(key.Marshal()) != string(allowed.Marshal()) {
				return nil, io.ErrUnexpectedEOF
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
			if err != nil {
				conn.Close()
				return
			}
			go ssh.DiscardRequests(reqs)
			for newCh := range chans {
				if newCh.ChannelType() != "session" {
					_ = newCh.Reject(ssh.UnknownChannelType, "session only")
					continue
				}
				ch, requests, err := newCh.Accept()
				if err != nil {
					continue
				}
				go runReceivePackChannel(ch, requests, repoDir)
			}
			_ = sc.Wait()
		}(conn)
	}
}

func runReceivePackChannel(ch ssh.Channel, requests <-chan *ssh.Request, repoDir string) {
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
		done := make(chan struct{})
		go func() { _, _ = io.Copy(stdin, ch); _ = stdin.Close(); close(done) }()
		_, _ = io.Copy(ch, stdout)
		<-done
		waitErr := cmd.Wait()
		status := uint32(0)
		if waitErr != nil {
			status = 1
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}
