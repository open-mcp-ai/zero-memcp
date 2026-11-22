package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 本地完全不做 commit，推 Git 仓库时才把当前工作层折成快照。
//
// 架构定位：节点间流动的是命令流，工作层是唯一日常状态。commit 不是
// 本地要维护的对象，而是「给外部 Git 仓库看」那一刻的产物。
//
// 这个测试把自动 commit 彻底关掉（interval=0 count=0），写一条记忆
// 让它躺在脏层里，然后对 Git 远端跑一轮 sync：必须当场折叠并推上去，
// 而不是报「N uncommitted change(s); commit before syncing」。
func TestGitPushSnapshotsDirtyWorkLayer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dataDir := t.TempDir()
	hubDir := filepath.Join(t.TempDir(), "hub.git")
	runGit(t, t.TempDir(), "init", "--bare", hubDir)

	cfg := fmt.Sprintf(`
[server]
data = %q

[commit]
interval = "0s"
count    = 0

[sync]
interval = "1h"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project   = "daily"
name      = "github"
url       = %q
push      = ["main.record"]
auto_sync = true
`, dataDir, hubDir)
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// 本地仓库先建好并种下基线（基线必须已 commit，否则远端没有可快进的祖先）。
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main.record", "seed.md", []byte("seed\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main.record", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:    dataDir,
		ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:     "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// 写一条记忆，**不 commit**——它就躺在脏层里。
	if err := repo.Write("main.record", "uncommitted.md", []byte("还没说出口\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if got := repo.Pending(); got == 0 {
		t.Fatal("测试前提：此时必须有未提交的脏层")
	}

	// 对 Git 远端跑一轮同步。
	cf := srv.cfgFilePtr.Load()
	if cf == nil || len(cf.Remotes) == 0 {
		t.Fatal("配置里应有一条 remote")
	}
	rm := cf.Remotes[0]
	srv.syncOnce("daily", rm)

	// ① 本地脏层被折成了 commit。
	if got := repo.Pending(); got != 0 {
		t.Fatalf("同步后脏层应被折叠，仍有 %d 条未提交", got)
	}

	// ② 远端真的收到了这条记忆的内容。
	cloneDir := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", "--branch", "main.record", hubDir, cloneDir)
	data, err := os.ReadFile(filepath.Join(cloneDir, "uncommitted.md"))
	if err != nil {
		t.Fatalf("远端应包含未提交的那条记忆: %v", err)
	}
	if string(data) != "还没说出口\n" {
		t.Fatalf("远端内容不符: %q", string(data))
	}
}
