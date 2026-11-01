package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// gitEnv 给测试里的 git 调用一套固定身份，避免依赖本机 git 配置。
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_TERMINAL_PROMPT=0",
	)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newRemoteServer 造一个带远端配置的服务端，并准备一个裸仓库。
func newRemoteServer(t *testing.T) (srv *Server, dataDir, remoteDir string) {
	t.Helper()
	dataDir = t.TempDir()
	remoteDir = filepath.Join(t.TempDir(), "remote.git")

	cmd := exec.Command("git", "init", "--bare", "--initial-branch=main", remoteDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init remote: %v %s", err, out)
	}

	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[user]]
name = "limited"

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwx------"

[[remote]]
project = "blackegg/me"
name    = "origin"
url     = "__URL__"
push    = ["main"]

[[remote]]
project = "blackegg/me"
name    = "mirror"
url     = "__URL__"
fetch_only = true
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	cfg = strings.ReplaceAll(cfg, "__URL__", remoteDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
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
	t.Cleanup(func() { _ = srv.Close() })

	// 项目仓库必须存在：gitstore 不自动创建。
	repoDir := filepath.Join(dataDir, "repos", "blackegg", "me.git")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "init", "--bare", "--initial-branch=main", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init local: %v %s", err, out)
	}
	return srv, dataDir, remoteDir
}

func TestRemoteVerbListsDeclaredRemotes(t *testing.T) {
	srv, dataDir, remoteDir := newRemoteServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "node-a", Cap: "rwx"})

	out, err := callMCP(t, srv, tok, "remote blackegg/me")
	if err != nil {
		t.Fatalf("remote 失败: %v", err)
	}
	if !strings.Contains(out, "origin") || !strings.Contains(out, "mirror") {
		t.Fatalf("应列出两个远端，实际: %s", out)
	}
	// 只读远端必须标出来
	if !strings.Contains(out, "只读") {
		t.Fatalf("mirror 应标记为只读: %s", out)
	}
	// 脱敏后的 URL 里不应出现凭证（本测试 URL 本就没有）
	if strings.Contains(out, remoteDir) == false {
		t.Fatalf("应显示远端地址: %s", out)
	}
}

func TestPushRequiresExplicitBranchAndRespectsFetchOnly(t *testing.T) {
	srv, dataDir, _ := newRemoteServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "node-a", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "push blackegg/me origin"); err == nil {
		t.Fatal("不指定分支应被拒绝")
	}
	if _, err := callMCP(t, srv, tok, "push blackegg/me mirror main"); err == nil {
		t.Fatal("只读远端不应接受 push")
	}
}

func TestPushRefusesWhenPendingCommits(t *testing.T) {
	srv, dataDir, _ := newRemoteServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "node-a", Cap: "rwx"})

	// 写一条但不提交
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main", "pending.md", []byte("uncommitted"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	out, err := callMCP(t, srv, tok, "push blackegg/me origin main")
	if err == nil {
		t.Fatalf("有未提交内容时 push 应被拒绝，实际: %s", out)
	}
	if !strings.Contains(err.Error(), "未提交") {
		t.Fatalf("拒绝理由应说明有未提交内容: %v", err)
	}
}

func TestFetchAndSyncAgainstLocalRemote(t *testing.T) {
	srv, dataDir, remoteDir := newRemoteServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "node-a", Cap: "rwx"})

	// 让远端先有一个 main 提交
	work := t.TempDir()
	cmd := exec.Command("git", "init", "--initial-branch=main", work)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init work: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(work, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "init")
	runGit(t, work, "push", remoteDir, "main")

	out, err := callMCP(t, srv, tok, "fetch blackegg/me origin")
	if err != nil {
		t.Fatalf("fetch 失败: %v (%s)", err, out)
	}
	if !strings.Contains(out, "main") {
		t.Fatalf("fetch 结果应包含 main: %s", out)
	}

	syncOut, err := callMCP(t, srv, tok, "sync blackegg/me origin")
	if err != nil {
		t.Fatalf("sync 失败: %v", err)
	}
	if !strings.Contains(syncOut, "remote-only") && !strings.Contains(syncOut, "behind") {
		t.Fatalf("sync 应报告本地落后或仅远端存在: %s", syncOut)
	}
}

// 受限凭证不能对不可见项目做远端操作。
func TestRemoteOpsRespectProjectVisibility(t *testing.T) {
	srv, dataDir, _ := newRemoteServer(t)
	// 挂载表里只有别的项目
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "limited",
		Name:  "node-ltd",
		Cap:   "rw",
		Mount: []auth.MountPoint{{Project: "blackegg/other", At: "/"}},
	})

	if _, err := callMCP(t, srv, tok, "remote blackegg/me"); err == nil {
		t.Fatal("不可见项目不应能列远端")
	}
	if _, err := callMCP(t, srv, tok, "fetch blackegg/me origin"); err == nil {
		t.Fatal("不可见项目不应能 fetch")
	}
}
