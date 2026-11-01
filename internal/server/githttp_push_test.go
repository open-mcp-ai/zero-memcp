package server

import (
	"fmt"
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
