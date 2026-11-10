package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 外部 push 也必须广播资源变更。
//
// 踩过的坑：checkExternalUpdates 只做 Reload，日志却写着
// "broadcasting tree changes"。于是另一台机器 push 上来、人手 git commit、
// 第三方 bot 直接推仓库 —— 订阅者收不到任何事件，会话永远不被唤醒。
// MCP 写入路径有 publishTreeChange 兜着，只有这条外部路径漏了。
func TestExternalBranchAdvancePublishesEvent(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

[[user]]
name = "zero"
root = true

[[project]]
name  = "BlackEggAI/zero"
owner = "zero"
mode  = "rwx------"
`
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(dataDir, "repos", "BlackEggAI", "zero.git")
	repo, err := gitstore.Init(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main.chat", "a.md", []byte("hello"), gitstore.ModeContent); err != nil {
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
	// 让水位与当前 heads 对齐（相当于进程正常运行时的状态）
	for k, v := range srv.treeHeadsSnapshot() {
		srv.lastCommits.Store(k, v.Commit)
	}

	c := srv.rootCaller()
	sub := srv.resources.add(c, []string{treeURI("BlackEggAI/zero", "main.chat", "")})
	if sub == nil {
		t.Fatal("订阅失败")
	}

	// 直接喂一个"外面推进了 main.chat"的快照：不依赖项目注册表，
	// 只锁死「外部推进 → 广播 + origin=external」这一条行为。
	key := treeHeadKey("BlackEggAI/zero", "main.chat")
	current := map[string]branchHead{
		key: {Project: "BlackEggAI/zero", Branch: "main.chat", Commit: "deadbeef"},
	}
	srv.publishExternalTreeChanges(current)

	select {
	case ev := <-sub.ch:
		if ev.URI == "" {
			t.Fatalf("事件缺少 uri")
		}
		if ev.Origin.Identity != "external" {
			t.Fatalf("外部推进的 origin.identity 应标记 external, 实际 %q", ev.Origin.Identity)
		}
		if ev.Commit != "deadbeef" {
			t.Fatalf("事件 commit 不符: %s", ev.Commit)
		}
		t.Logf("收到事件 uri=%s commit=%s origin=%s", ev.URI, ev.Commit, ev.Origin.Identity)
	case <-time.After(2 * time.Second):
		t.Fatal("外部推进分支后没有收到资源变更事件")
	}
}
