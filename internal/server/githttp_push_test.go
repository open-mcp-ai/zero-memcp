package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 真实外部 git push 到我们的服务端：
// 外部工作树 commit → git push http://... → 我们的仓库前进 → Reload 重建索引。
func TestExternalGitPushIntoOurServer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[listen]]
addr  = "127.0.0.1:0"
allow = ["*", "git-upload-pack", "git-receive-pack"]

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`, dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "init.md", []byte("种子\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	seed, err := repo.Commit("zero", "seed")
	if err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()
	addr := waitForServerAddr(t, srv)

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "writer", Cap: "rwx"})

	// 在外部 clone 出一个工作树
	workDir := filepath.Join(t.TempDir(), "work")
	remoteURL := fmt.Sprintf("http://zero:%s@%s/repos/daily.git", tok, addr)
	runGit(t, t.TempDir(), "clone", "-q", remoteURL, workDir)

	// 在外部写新内容、commit、push 回来
	if err := osWrite(filepath.Join(workDir, "external.md"), "外部注入的记忆\n"); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "inject")
	runGit(t, workDir, "push", "-q", "origin", "zero")

	// 确认服务端的 ref 真的前进了，并且读得到新内容
	newHead, err := repo.Resolve("zero")
	if err != nil || newHead == seed {
		t.Fatalf("服务端分支应前进，实际 %s == %s", newHead, seed)
	}
	body, err := repo.Read("zero", "external.md")
	if err != nil || string(body) != "外部注入的记忆\n" {
		t.Fatalf("读回内容不符: %v %q", err, string(body))
	}

	// 索引应当也被 Reload 重建，能被 recall 搜到
	out, err := callMCP(t, srv, tok, "recall 外部注入")
	if err != nil || !strings.Contains(out, "external.md") {
		t.Fatalf("push 后记忆应立即可搜: %v (%s)", err, out)
	}
}

// 端口未显式开放 git-receive-pack 时，push 必须被 403 挡住。
func TestExternalGitPushBlockedWhenPortDisallows(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[listen]]
addr  = "127.0.0.1:0"
allow = ["git-upload-pack"]

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`, dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.Write("zero", "a.md", []byte("a\n"), gitstore.ModeContent)
	_, _ = repo.Commit("zero", "seed")

	srv, err := New(Config{DataDir: dataDir, ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"), EnforceAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()
	addr := waitForServerAddr(t, srv)

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})
	remoteURL := fmt.Sprintf("http://zero:%s@%s/repos/daily.git", tok, addr)

	workDir := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "clone", "-q", remoteURL, workDir)

	_ = osWrite(filepath.Join(workDir, "b.md"), "b\n")
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "b")

	cmd := exec.Command("git", "-C", workDir, "push", "-q", "origin", "zero")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("未开 receive-pack 的端口应拒绝 push: %s", out)
	}
}

// 凭证只有只读权限时，即使端口开了也必须在 receive-pack 处被拒。
func TestExternalGitPushRejectedByScopedToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[listen]]
addr  = "127.0.0.1:0"
allow = ["git-upload-pack", "git-receive-pack"]

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`, dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.Write("zero", "a.md", []byte("a\n"), gitstore.ModeContent)
	_, _ = repo.Commit("zero", "seed")

	srv, err := New(Config{DataDir: dataDir, ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"), EnforceAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()
	addr := waitForServerAddr(t, srv)

	roTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "ro", Cap: "r"})
	remoteURL := fmt.Sprintf("http://zero:%s@%s/repos/daily.git", roTok, addr)

	workDir := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "clone", "-q", remoteURL, workDir)

	_ = osWrite(filepath.Join(workDir, "b.md"), "b\n")
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "b")

	cmd := exec.Command("git", "-C", workDir, "push", "-q", "origin", "zero")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("只读凭证应被拒绝 push: %s", out)
	}
}

func waitForServerAddr(t *testing.T, s *Server) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ln := s.getListener(); ln != nil {
			return ln.Addr().String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("服务未就绪")
	return ""
}

// 真实 git 的 --force push 不能覆盖服务端已有历史，即使 token 是 root。
func TestExternalGitForcePushRejected(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[listen]]
addr  = "127.0.0.1:0"
allow = ["git-upload-pack", "git-receive-pack"]

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`, dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.Write("zero", "base.md", []byte("base\n"), gitstore.ModeContent)
	seed, err := repo.Commit("zero", "seed")
	if err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{DataDir: dataDir, ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"), EnforceAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()
	addr := waitForServerAddr(t, srv)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "writer", Cap: "rwx"})
	url := fmt.Sprintf("http://zero:%s@%s/repos/daily.git", tok, addr)
	workDir := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "clone", "-q", url, workDir)

	// 本地历史分叉出一个未在服务端的 root commit。
	cmd := exec.Command("git", "-C", workDir, "checkout", "--orphan", "replacement")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout orphan: %v %s", err, out)
	}
	_ = osWrite(filepath.Join(workDir, "replacement.md"), "replace\n")
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "replacement")
	cmd = exec.Command("git", "-C", workDir, "push", "--force", "origin", "replacement:zero")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("非快进推送应拒绝: %s", out)
	}
	if got, _ := repo.Resolve("zero"); got != seed {
		t.Fatalf("拒绝之后远端历史必须保持 %s，实际 %s", seed, got)
	}
}

// 不存在的项目必须 404，不能报成 403「不在可见范围」。
//
// 受影响的第一个场景就是 peer 同步：新节点入网时对端仓库经常还不存在，
// 报 403 会让人去查 token、挂载、cap，而真因是「对端还没写过东西」。
func TestGitHTTPMissingProjectIs404(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[listen]]
addr  = "127.0.0.1:0"
allow = ["*", "git-upload-pack", "git-receive-pack"]

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`, dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "init.md", []byte("种子\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()
	addr := waitForServerAddr(t, srv)

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "reader", Cap: "rwx"})

	for _, project := range []string{"daily/not-yet", "no-such-project"} {
		url := fmt.Sprintf("http://zero:%s@%s/repos/%s.git/info/refs?service=git-upload-pack", tok, addr, project)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("项目 %q 不存在时应返回 404，得到 %d: %s", project, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
}

// 向「配置里已声明、但磁盘上还没有仓库」的项目推第一次，必须成功。
//
// 这是新节点入网的第一幕：hub 刚起、库是空的，spoke 要把全量历史推上来。
// 两处都曾挡住它：
//
//  1. 接收端用 Get（读语义，不建库）→ 报「仓库不存在」。修成推送方向用 Ensure。
//  2. 只放行 POST /git-receive-pack，忘了 git 会先发
//     GET /info/refs?service=git-receive-pack 探测 → 客户端在探测那步
//     就拿到 404，永远走不到推送。
//
// 第 2 条尤其阴——修完 Ensure 单测能过、真机仍推不上去，因为单测直接打
// POST，而 git 客户端永远先探测。
func TestGitHTTPPushCreatesDeclaredButEmptyProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[listen]]
addr  = "127.0.0.1:0"
allow = ["*", "git-upload-pack", "git-receive-pack"]

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`, dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	// 注意：这里**故意不**初始化 repos/daily.git —— 项目声明了，仓库还不存在。

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()
	addr := waitForServerAddr(t, srv)

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "writer", Cap: "rwx"})

	remoteURL := fmt.Sprintf("http://zero:%s@%s/repos/daily.git", tok, addr)

	// 从一个本地种子仓推过去：起点必须由客户端提供（对端还没有任何东西）
	seedDir := filepath.Join(t.TempDir(), "seed")
	runGit(t, "", "init", "-q", "-b", "main", seedDir)
	runGit(t, seedDir, "config", "user.email", "t@example.com")
	runGit(t, seedDir, "config", "user.name", "t")
	if err := osWrite(filepath.Join(seedDir, "a.md"), "第一篇\n"); err != nil {
		t.Fatal(err)
	}
	runGit(t, seedDir, "add", "-A")
	runGit(t, seedDir, "commit", "-q", "-m", "seed")
	runGit(t, seedDir, "remote", "add", "origin", remoteURL)
	runGit(t, seedDir, "push", "-q", "origin", "main")

	// 推完之后仓库必须真的存在，而且分支指针指向那个提交
	if _, err := os.Stat(filepath.Join(dataDir, "repos", "daily.git")); err != nil {
		t.Fatalf("推送后仍没有仓库目录: %v", err)
	}
	// 显式 -b main：仓库 HEAD 指向 zero（DefaultBranch），而这里推的是 main。
	// 裸仓 HEAD 指向一个还不存在的分支时，clone 会警告并留下空工作树——
	// 这与真实 git（init --bare 后推别的分支）的行为一致，不是本服务的缺陷。
	workDir2 := filepath.Join(t.TempDir(), "pull")
	runGit(t, t.TempDir(), "clone", "-q", "-b", "main", remoteURL, workDir2)
	data, err := os.ReadFile(filepath.Join(workDir2, "a.md"))
	if err != nil {
		t.Fatalf("clone 回来的仓库里没有 a.md: %v", err)
	}
	if strings.TrimSpace(string(data)) != "第一篇" {
		t.Fatalf("内容不一致: %q", string(data))
	}
}
