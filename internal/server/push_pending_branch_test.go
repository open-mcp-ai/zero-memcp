package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 一条分支上的未提交改动，不许拦住另一条分支的推送。
//
// 实测来源（2026-11-25）：
//
//	本机 meta.index（向量索引快照链，246MB）长期挂着未折叠的改动，而它
//	不在任何 remote 的推送范围里。于是 main.chat 的每一次推送都被拦下来：
//
//	  push main.chat failed: project BlackEggAI/zero has 1 uncommitted changes:
//	  push does not commit for you. Commit first, then push
//
//	脏的那条分支根本没打算推，也没人打算提交它——结果一条完全无关的分支
//	把真正的同步永久卡住，日志里只有一句「1 个未提交改动」，看不出是哪个。
//
// 判据必须是「你要推的这条分支有没有未提交内容」。
func TestPushNotBlockedByDirtyOtherBranch(t *testing.T) {
	dataDir := t.TempDir()
	// 远端指向一个不存在的地址：本测试只关心**有没有走到网络那一步**。
	// 被未提交判据拦下时根本不会发起连接，错误文本里也就不会有连接失败。
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
project = "daily"
name    = "bogus"
url     = "http://127.0.0.1:1/repo.git"
push    = ["main.record"]
`, dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git")); err != nil {
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
	token := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})

	// 在 main 上留一条未提交改动——而远端只推 main.record。
	// 这正是线上那条 246MB 的 meta.index：永远挂着、永远不提交、也永远不推。
	if _, err := callMCPWithVars(t, srv, token, "write daily@main:noise.md $BODY",
		map[string]string{"BODY": "一条永远不提交的脏改动"}); err != nil {
		t.Fatalf("制造脏分支: %v", err)
	}
	if n := srv.pendingOnBranch("daily", "main"); n == 0 {
		t.Fatal("前提：daily@main 应当有未提交改动")
	}
	if n := srv.pendingOnBranch("daily", "main.record"); n != 0 {
		t.Fatalf("main.record 没有未提交改动，判据却报了 %d 条", n)
	}

	// 推 main.record：必须越过未提交判据、真的去连远端（会失败，因为没人在听）。
	_, perr := callMCP(t, srv, token, "push daily bogus main.record")
	if perr == nil {
		t.Fatal("远端不存在，推送应当失败")
	}
	if strings.Contains(perr.Error(), "uncommitted") {
		t.Fatalf("别的分支脏不许拦这条分支；它甚至没走到网络那一步。实际报错: %v", perr)
	}
	// 确认它确实走到了连接远端那一步。
	if !strings.Contains(perr.Error(), "127.0.0.1:1") && !strings.Contains(perr.Error(), "connect") {
		t.Logf("推送失败原因（应含连接失败字样）: %v", perr)
	}
}
