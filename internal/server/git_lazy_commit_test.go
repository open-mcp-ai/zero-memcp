package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
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
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(withIndex(cfg)), 0o644); err != nil {
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
	srv.syncOnce("daily", rm, false)

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
	// Windows 上 git 检出会把行尾写成 CRLF，内容本身是一样的；
	// 比较的是「这条记忆有没有到」而不是行尾风格。
	if got := strings.ReplaceAll(string(data), "\r\n", "\n"); got != "还没说出口\n" {
		t.Fatalf("远端内容不符: %q", got)
	}
}

// 推送时折叠出的 commit 必须被记住，不能被当成「外部 push」。
//
// 实测故障（2026-11-23）：syncOnce 直接调 repo.Commit 折叠脏层后，没把
// 新 head 记进 lastCommits。下一轮 checkExternalUpdates 看到 head 变了，
// 判定为外部推进，于是 refreshBranches 全量重扫那条分支——
// weft@main.record 有 17,781 个路径，重扫期间写入请求全被堵住，
// 实测写一条 25 字节的记忆卡了 30 秒。
//
// 这条测试直接断言「折叠后 lastCommits 里记的就是当前 head」——
// 这是「自己的动作不会被误判成别人的」这条不变量的可执行形式。
func TestLazyCommitRecordsHeadSoItIsNotSeenAsExternal(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(withIndex(cfg)), 0o644); err != nil {
		t.Fatal(err)
	}

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

	// 服务器先把当前 head 记下来（正常启动流程会做这件事）。
	srv.rememberTreeHeads()

	// 写一条不 commit 的记忆——**走服务端自己的写入路径**。
	//
	// 不能用测试自己的 repo 句柄写：句柄各自缓存脏层视图，服务端看不到
	// 另一个句柄写的文件（生产里只有一个句柄，不存在这个问题）。
	// 这条测试要验证的是服务端的行为，所以写入也必须经过服务端。
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	repoDir := filepath.Join(dataDir, "repos", "daily.git")
	before, oerr := gitstore.Open(repoDir)
	if oerr != nil {
		t.Fatal(oerr)
	}
	headBefore, err := before.Resolve("main.record")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write daily@main.record/later.md $TEXT",
			"vars": map[string]string{"TEXT": "后来写的"},
		},
	}); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	cf := srv.cfgFilePtr.Load()
	if cf == nil || len(cf.Remotes) == 0 {
		t.Fatal("配置里应有一条 remote")
	}
	srv.syncOnce("daily", cf.Remotes[0], false)

	// 先证明折叠真的发生了：head 必须动过。
	// 没有这一步，一个「根本没折叠」的实现也能让下面的断言平凡通过
	// （旧 head == 记录的 head），测试就变成了摆设。
	// 用新句柄读：测试手里的 repo 有自己的 ref 缓存，
	// 服务端通过它自己的句柄提交，两边的缓存不会互相看见。
	fresh, err := gitstore.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := fresh.Resolve("main.record")
	if err != nil || head == "" {
		t.Fatalf("解析 main.record: %v", err)
	}
	if head == headBefore {
		t.Fatalf("同步后 head 没变（%s）：说明折叠根本没发生，本测试失去意义",
			shortSHA(head))
	}

	// 不变量：记录里的 head 必须等于当前 head。
	val, ok := srv.lastCommits.Load(treeHeadKey("daily", "main.record"))
	if !ok {
		t.Fatal("折叠之后必须记下 head，否则会被当成外部推进")
	}
	if val.(string) != head {
		t.Fatalf("记录的 head %s != 当前 head %s：下一轮会触发全分支重扫",
			shortSHA(val.(string)), shortSHA(head))
	}
}
