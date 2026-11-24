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
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// fetchAllBranches 是测试里「全取」的显式声明，等价于配置里的 pull = ["*"]。
// 取回范围必须显式给出，所以连测试也不能省略这个参数。
var fetchAllBranches = []string{"*"}

// seenBodies 记录服务端收到的请求体，用来证明客户端真的走了 v2 命令。
type seenBodies struct {
	list atomic.Pointer[[]string]
}

func (b *seenBodies) add(raw string) {
	for {
		cur := b.snapshot()
		next := append([]string(nil), cur...)
		next = append(next, raw)
		if b.list.CompareAndSwap(b.list.Load(), &next) {
			return
		}
	}
}

func (b *seenBodies) snapshot() []string {
	if p := b.list.Load(); p != nil {
		return *p
	}
	return nil
}

func (b *seenBodies) has(sub string) bool {
	for _, item := range b.snapshot() {
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
	res, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", fetchAllBranches, 20*time.Second)
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
	_, err = FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "wrong", fetchAllBranches, 5*time.Second)
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
	res, err := FetchSSH(context.Background(), local, url, "origin", string(pem.EncodeToMemory(block)), ssh.FingerprintSHA256(hostSigner.PublicKey()), fetchAllBranches, 20*time.Second)
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
	if _, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", fetchAllBranches, 20*time.Second); err != nil {
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

	res, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", fetchAllBranches, 20*time.Second)
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

func TestHTTPFetchPullsOnlyExplicitBranches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)

	// 造三条分支：main、main.record 和 meta.index
	work := filepath.Join(root, "work")
	runTestGit(t, "init", work)
	runGitIn(t, work, "checkout", "-b", "main")
	writeFileTest(t, filepath.Join(work, "main.txt"), `hello main
`)
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "commit on main")
	runGitIn(t, work, "push", remoteDir, "main")
	_ = gitOutTest(t, work, "rev-parse", "HEAD") // main 的 head：不参与断言，只用来确认推送成功

	runGitIn(t, work, "checkout", "-b", "main.record")
	writeFileTest(t, filepath.Join(work, "record.txt"), `hello record
`)
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "commit on main.record")
	runGitIn(t, work, "push", remoteDir, "main.record")
	headRecord := gitOutTest(t, work, "rev-parse", "HEAD")

	runGitIn(t, work, "checkout", "-b", "meta.index")
	writeFileTest(t, filepath.Join(work, "index.txt"), `hello meta.index: massive snapshot
`)
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "commit on meta.index")
	runGitIn(t, work, "push", remoteDir, "meta.index")
	headMeta := gitOutTest(t, work, "rev-parse", "HEAD")

	srv := uploadPackHTTP(t, remoteDir, "reader", "tok")
	defer srv.Close()

	// 客户端只显式要 main.record：meta.index 与 main 绝对不能被拉下来
	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", []string{"main.record"}, 20*time.Second)
	if err != nil {
		t.Fatalf("fetch 失败: %v", err)
	}

	// 1. 结果里只有 main.record
	if len(res.Branches) != 1 || res.Branches["main.record"] != headRecord {
		t.Fatalf("返回分支应只有 main.record: got %v, want {main.record: %s}", res.Branches, headRecord)
	}
	if _, ok := res.Branches["meta.index"]; ok {
		t.Fatal("meta.index 绝对不该出现在返回结果中")
	}
	if _, ok := res.Branches["main"]; ok {
		t.Fatal("main 没要，绝对不该出现在返回结果中")
	}

	// 2. 本地对象库里只有 main.record 的对象，绝对不能有 meta.index 的 blob
	if !local.HasObject(headRecord) {
		t.Fatal("要的分支 main.record 的 head 应在本地")
	}
	if local.HasObject(headMeta) {
		t.Fatalf("未请求的 meta.index head %s 竟然被拉进了本地对象库！打包隔离失效", headMeta)
	}

	// 3. 跟踪引用只写了 origin/main.record
	if got := local.RemoteRef("origin", "main.record"); got != headRecord {
		t.Fatalf("refs/remotes/origin/main.record = %q, want %q", got, headRecord)
	}
	if got := local.RemoteRef("origin", "meta.index"); got != "" {
		t.Fatalf("refs/remotes/origin/meta.index 不该存在: got %q", got)
	}
	if got := local.RemoteRef("origin", "main"); got != "" {
		t.Fatalf("refs/remotes/origin/main 不该存在: got %q", got)
	}
}

func TestHTTPFetchEmptyBranchesPullsNothing(t *testing.T) {
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
	// 空白名单：不拉任何东西
	res, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("空列表应安静返回: %v", err)
	}
	if len(res.Branches) != 0 {
		t.Fatalf("空白名单不该拉下任何分支: %v", res.Branches)
	}
}

// 两阶段：对象已经在库里时，没要过的分支也绝不能凭空多出一个跟踪引用。
//
// 为什么需要这一条（对象是内容寻址的，HasObject 拦不住它）：第一轮把两条
// 分支都取回来，删掉 meta.index 的跟踪引用但保留对象；第二轮只取
// main.record，对端这次没有任何新对象要发，pack 是空的，但广告里仍然有
// meta.index 的 tip——而那个 tip 的对象**在库里**（第一轮留下的）。
// 只看 HasObject 的实现会在这里把 refs/remotes/origin/meta.index 写回去，
// 于是下一轮 merge 就多出一条从没请求过的分支当真值。
func TestApplyFetchNeverWritesUnrequestedRemoteRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)

	work := filepath.Join(root, "work")
	runTestGit(t, "init", work)
	runGitIn(t, work, "checkout", "-b", "main.record")
	writeFileTest(t, filepath.Join(work, "a.md"), `甲
`)
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "a")
	runGitIn(t, work, "push", remoteDir, "main.record")
	headRecord := gitOutTest(t, work, "rev-parse", "HEAD")

	runGitIn(t, work, "checkout", "-b", "meta.index")
	writeFileTest(t, filepath.Join(work, "b.md"), `乙
`)
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "b")
	runGitIn(t, work, "push", remoteDir, "meta.index")
	headMeta := gitOutTest(t, work, "rev-parse", "HEAD")

	srv := uploadPackHTTP(t, remoteDir, "reader", "tok")
	defer srv.Close()

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}

	// ── 第一轮：两条都取，对象与引用都落地 ──
	if _, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok",
		[]string{"main.record", "meta.index"}, 20*time.Second); err != nil {
		t.Fatalf("第一轮 fetch 失败: %v", err)
	}
	if got := local.RemoteRef("origin", "meta.index"); got != headMeta {
		t.Fatalf("前置条件：第一轮应写 origin/meta.index = %s，实际 %q", headMeta, got)
	}
	if !local.HasObject(headMeta) {
		t.Fatal("前置条件：meta.index 的对象应在库里")
	}

	// 删掉跟踪引用，但**保留对象**——这正是让 HasObject 拦不住的那一步。
	if err := local.DeleteRemoteRef("origin", "meta.index"); err != nil {
		t.Fatal(err)
	}

	// ── 第二轮：只取 main.record ──
	res, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok",
		[]string{"main.record"}, 20*time.Second)
	if err != nil {
		t.Fatalf("第二轮 fetch 失败: %v", err)
	}
	if got := local.RemoteRef("origin", "meta.index"); got != "" {
		t.Fatalf("meta.index 没要过，跟踪引用绝不该被写回，实际 %q", got)
	}
	if _, ok := res.Branches["meta.index"]; ok {
		t.Fatalf("返回结果不该包含 meta.index: %v", res.Branches)
	}
	if got := local.RemoteRef("origin", "main.record"); got != headRecord {
		t.Fatalf("要的分支引用应保持 %s，实际 %q", headRecord, got)
	}
}

// 已对齐时再取一次必须成功，而且不能带来任何新对象。
//
// 为什么这条必须单独钉住（2026-11-24 部署短路到 3060 当场撞出来）：
// 差集为空时服务端发的是「0 个对象的 PACK」，客户端如果在读完 ACK/NAK 后
// 直接撞上流末尾，就会报 `read pack: EOF`——把「本来就没有东西要传」
// 误报成传输中断。auto-sync 每轮都会走这条路径，一旦它失败，
// 同步永远停在「上一轮成功」那一刻，而且报出来的是一行看起来像网络问题的错。
func TestHTTPFetchWhenAlreadyUpToDateSucceeds(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)

	work := filepath.Join(root, "work")
	runTestGit(t, "init", work)
	runGitIn(t, work, "checkout", "-b", "main.record")
	writeFileTest(t, filepath.Join(work, "a.md"), `甲
`)
	runGitIn(t, work, "add", ".")
	runGitIn(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "a")
	runGitIn(t, work, "push", remoteDir, "main.record")
	head := gitOutTest(t, work, "rev-parse", "HEAD")

	srv := uploadPackHTTP(t, remoteDir, "reader", "tok")
	defer srv.Close()

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}

	// 第一轮：真的取回对象
	first, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", fetchAllBranches, 20*time.Second)
	if err != nil {
		t.Fatalf("第一轮 fetch 失败: %v", err)
	}
	if first.Branches["main.record"] != head {
		t.Fatalf("第一轮应取到 main.record=%s，实际 %v", head, first.Branches)
	}

	// 第二轮：两端已对齐，服务端会短路并发一个空 pack。
	// 这一轮必须成功，不能报 `read pack: EOF`。
	second, err := FetchHTTP(context.Background(), local, srv.URL+"/repo.git", "origin", "reader", "tok", fetchAllBranches, 20*time.Second)
	if err != nil {
		t.Fatalf("已对齐时再 fetch 必须成功，实际失败: %v", err)
	}
	if second.Branches["main.record"] != head {
		t.Fatalf("已对齐时分支指针应保持不变: %v", second.Branches)
	}
	if second.Written != 0 {
		t.Fatalf("已对齐时不该写入任何新对象，实际写了 %d 个", second.Written)
	}
	if got := local.RemoteRef("origin", "main.record"); got != head {
		t.Fatalf("refs/remotes/origin/main.record 应保持 %s，实际 %q", head, got)
	}
}
