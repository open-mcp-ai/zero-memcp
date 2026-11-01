package gittransport

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// seenBodies 记录服务端收到的请求体，用来证明客户端真的走了 v2 命令。
type seenBodies struct {
	mu   sync.Mutex
	list []string
}

func (b *seenBodies) add(raw string) {
	b.mu.Lock()
	b.list = append(b.list, raw)
	b.mu.Unlock()
}

func (b *seenBodies) has(sub string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, item := range b.list {
		if strings.Contains(item, sub) {
			return true
		}
	}
	return false
}

type smartServer struct {
	*httptest.Server
	bodies *seenBodies
}

// uploadPackHTTP 扮演远端：只读 smart HTTP，把请求转给 git upload-pack。
func uploadPackHTTP(t *testing.T, repoDir, user, token string) *smartServer {
	t.Helper()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != want {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	bodies := &seenBodies{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") != "git-upload-pack" {
			http.Error(w, "unsupported", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write(append([]byte("001e# service=git-upload-pack\n0000"), uploadPackRun(t, repoDir, "info/refs", r)...))
	}))
	mux.HandleFunc("/repo.git/git-upload-pack", guard(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		bodies.add(string(raw))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_, _ = w.Write(uploadPackRun(t, repoDir, "git-upload-pack", r))
	}))
	return &smartServer{Server: httptest.NewServer(mux), bodies: bodies}
}

func uploadPackRun(t *testing.T, repoDir, action string, r *http.Request) []byte {
	t.Helper()
	args := []string{"upload-pack", "--stateless-rpc"}
	if action == "info/refs" {
		args = append(args, "--advertise-refs")
	}
	args = append(args, repoDir)
	cmd := exec.Command("git", args...)
	if v := r.Header.Get("Git-Protocol"); v != "" {
		// smart HTTP 通过该请求头传递协议版本，服务端据此切到 v2。
		cmd.Env = append(cmd.Environ(), "GIT_PROTOCOL="+v)
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	cmd.Stdin = bytes.NewReader(raw)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s | body=%q", args, err, stderr.String(), string(raw))
	}
	return out
}

func TestHTTPFetchStoresObjectsAndRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)

	// 先造出真实历史
	work := filepath.Join(root, "work")
	runTestGit(t, "init", work)
	runGitIn(t, work, "checkout", "-b", "main")
	writeFileTest(t, filepath.Join(work, "memory.md"), "跨节点记忆\n")
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "first")
	runGitIn(t, work, "push", remoteDir, "main")
	head := gitOutTest(t, work, "rev-parse", "HEAD")

	srv := uploadPackHTTP(t, remoteDir, "reader", "tok")
	defer srv.Close()

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", 20*time.Second)
	if err != nil {
		t.Fatalf("HTTP fetch 失败: %v", err)
	}
	if res.Branches["main"] != head {
		t.Fatalf("分支 sha 不符: %v", res.Branches)
	}
	if !local.HasObject(head) {
		t.Fatal("取回后对象应可读")
	}
	if got := local.RemoteRef("origin", "main"); got != head {
		t.Fatalf("refs/remotes/origin/main = %q, want %q", got, head)
	}
	// 服务端应当收到过 ls-refs 命令——那是 v2 独有的请求，
	// 只有它出现，才说明这次真的走了 v2 而不是悄悄退回 v0。
	if !srv.bodies.has("command=ls-refs") {
		t.Fatal("未观察到 v2 ls-refs 命令，fetch 可能退回了 v0")
	}
}

func TestHTTPFetchRejectsBadCredentials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)
	srv := uploadPackHTTP(t, remoteDir, "reader", "tok")
	defer srv.Close()

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "wrong", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误凭证应被拒绝: %v", err)
	}
}

func writeFileTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOutTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		// worktree 用 -C
		cmd = exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err = cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return strings.TrimSpace(string(out))
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestSSHFetchStoresObjectsAndRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)
	work := filepath.Join(root, "work")
	runTestGit(t, "init", work)
	runGitIn(t, work, "checkout", "-b", "main")
	writeFileTest(t, filepath.Join(work, "m.md"), "记忆\n")
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "c1")
	runGitIn(t, work, "push", remoteDir, "main")
	head := gitOutTest(t, work, "rev-parse", "HEAD")

	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, _ := ssh.NewSignerFromKey(crypto.Signer(clientPriv))
	hostSigner, _ := ssh.NewSignerFromKey(crypto.Signer(hostPriv))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveGitSSH(t, ln, hostSigner, clientSigner.PublicKey(), remoteDir)

	block, err := ssh.MarshalPrivateKey(clientPriv, "k")
	if err != nil {
		t.Fatal(err)
	}
	url := "ssh://git@127.0.0.1:" + itoa(ln.Addr().(*net.TCPAddr).Port) + "/unused.git"

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := FetchSSH(context.Background(), local, url, "origin", string(pem.EncodeToMemory(block)), ssh.FingerprintSHA256(hostSigner.PublicKey()), 20*time.Second)
	if err != nil {
		t.Fatalf("SSH fetch 失败: %v", err)
	}
	if res.Branches["main"] != head {
		t.Fatalf("分支 sha 不符: %v", res.Branches)
	}
	if !local.HasObject(head) {
		t.Fatal("取回后对象应可读")
	}
	if got := local.RemoteRef("origin", "main"); got != head {
		t.Fatalf("refs/remotes/origin/main = %q", got)
	}
}

// serveGitSSH 是一个通用 SSH 服务端：exec 什么 git 子命令就跑什么。
func serveGitSSH(t *testing.T, l net.Listener, hostSigner ssh.Signer, allowed ssh.PublicKey, repoDir string) {
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
				go func(ch ssh.Channel, requests <-chan *ssh.Request) {
					defer ch.Close()
					// 收集 env：协议 v2 靠 GIT_PROTOCOL 开启，服务端要真的把它
					// 传给子进程，否则「v2 测试」其实一直跑在 v0 上。
					env := map[string]string{}
					for req := range requests {
						if req.Type == "env" {
							var e struct{ Name, Value string }
							if err := ssh.Unmarshal(req.Payload, &e); err == nil {
								env[e.Name] = e.Value
								_ = req.Reply(true, nil)
								continue
							}
							_ = req.Reply(false, nil)
							continue
						}
						if req.Type != "exec" {
							_ = req.Reply(false, nil)
							continue
						}
						var msg struct{ Command string }
						if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
							_ = req.Reply(false, nil)
							return
						}
						name := ""
						switch {
						case strings.HasPrefix(msg.Command, "git-upload-pack "):
							name = "upload-pack"
						case strings.HasPrefix(msg.Command, "git-receive-pack "):
							name = "receive-pack"
						default:
							_ = req.Reply(false, nil)
							return
						}
						_ = req.Reply(true, nil)
						cmd := exec.Command("git", name, repoDir)
						for k, v := range env {
							cmd.Env = append(cmd.Environ(), k+"="+v)
						}
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
						status := uint32(0)
						if err := cmd.Wait(); err != nil {
							status = 1
						}
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
						return
					}
				}(ch, requests)
			}
			_ = sc.Wait()
		}(conn)
	}
}

// 第二次 fetch 会带上 have：远端只送新增部分。
// 这里同时验证 thin/have 协商下对象仍然齐全（缺基对象会在这里暴露）。
func TestHTTPFetchIncrementalWithHaves(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)
	work := filepath.Join(root, "work")
	runTestGit(t, "init", work)
	runGitIn(t, work, "checkout", "-b", "main")
	writeFileTest(t, filepath.Join(work, "a.md"), "第一条\n")
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "c1")
	runGitIn(t, work, "push", remoteDir, "main")
	first := gitOutTest(t, work, "rev-parse", "HEAD")

	srv := uploadPackHTTP(t, remoteDir, "reader", "tok")
	defer srv.Close()

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", 20*time.Second); err != nil {
		t.Fatalf("首次 fetch 失败: %v", err)
	}
	// 本地要有一个分支，localHaves 才有东西可发。
	if err := local.SetRef("refs/heads/main", first); err != nil {
		t.Fatal(err)
	}

	writeFileTest(t, filepath.Join(work, "b.md"), "第二条\n")
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "c2")
	runGitIn(t, work, "push", remoteDir, "main")
	second := gitOutTest(t, work, "rev-parse", "HEAD")

	res, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", 20*time.Second)
	if err != nil {
		t.Fatalf("增量 fetch 失败: %v", err)
	}
	if res.Branches["main"] != second {
		t.Fatalf("分支应更新到 %s，实际 %v", second, res.Branches)
	}
	if !local.HasObject(second) || !local.HasObject(first) {
		t.Fatal("增量取回后新旧对象都应可读")
	}
	if local.RemoteRef("origin", "main") != second {
		t.Fatal("远端跟踪引用没更新")
	}
}
