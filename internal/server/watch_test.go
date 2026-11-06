package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// newWatchTestServer 建一个带 daily 项目的最小服务。
func newWatchTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`
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
	t.Cleanup(func() { srv.Close() })

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "seed.md", []byte("seed"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}
	return srv, dataDir
}

// TestRemindRecordsBaseline 锁定 remind 的新语义：
// 它记录「我关心这棵树」+ 当前分支头作为基线，不订阅、不推送。
func TestRemindRecordsBaseline(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "watcher", Cap: "rwx"})

	out, err := callMCP(t, srv, tok, "remind daily@zero/team/chat")
	if err != nil {
		t.Fatalf("remind: %v", err)
	}
	if !strings.Contains(out, "watching daily@zero/team/chat") {
		t.Fatalf("remind 输出应含被关注的树，实际: %s", out)
	}
	// 基线必须被记录（seed commit 存在，所以 since 非空）
	if !strings.Contains(out, "from ") {
		t.Fatalf("remind 应报告基线 commit，实际: %s", out)
	}

	// 列表能读回来
	list, err := callMCP(t, srv, tok, "remind")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "team/chat") {
		t.Fatalf("remind 列表应含该树，实际: %s", list)
	}
}

// TestStatusReportsWatchedTreeMoved 锁定 A+C 方案的核心：
// status 必须回答「我关注的树动了没有」，并给出取差量的命令。
//
// 这是原先 SSE 推送的位置。推送删掉之后，这个信息就是唯一能让
// 调用方发现「别处写了东西」的途径——它不能缺。
func TestStatusReportsWatchedTreeMoved(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "watcher", Cap: "rwx"})

	// 锚定（此时基线 = 当前头）
	if _, err := callMCP(t, srv, tok, "remind daily@zero"); err != nil {
		t.Fatal(err)
	}

	// 未变更时：status 应报 unchanged
	st, err := callMCP(t, srv, tok, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st, "watched:") || !strings.Contains(st, "unchanged") {
		t.Fatalf("刚锚定时应报 unchanged，实际:\n%s", st)
	}

	// 推进分支
	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "team/chat/01.md", []byte("new"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "advance"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}

	// 现在 status 必须报 MOVED，且给出 since 命令
	st, err = callMCP(t, srv, tok, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st, "MOVED") {
		t.Fatalf("分支推进后 status 应报 MOVED，实际:\n%s", st)
	}
	if !strings.Contains(st, "use: since") {
		t.Fatalf("status 应给出取差量的命令，实际:\n%s", st)
	}

	// 重新锚定后回到 unchanged
	if _, err := callMCP(t, srv, tok, "remind daily@zero"); err != nil {
		t.Fatal(err)
	}
	st, err = callMCP(t, srv, tok, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(st, "MOVED") {
		t.Fatalf("重新锚定后不应再报 MOVED，实际:\n%s", st)
	}
}

// TestWatchIsolatedBetweenIdentities 锁定隔离：
// 一张凭证的关注不该出现在另一张凭证的 status 里。
func TestWatchIsolatedBetweenIdentities(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tokA := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "agent-a", Cap: "rwx"})
	tokB := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "agent-b", Cap: "rwx"})

	if _, err := callMCP(t, srv, tokA, "remind daily@zero/team"); err != nil {
		t.Fatal(err)
	}
	stB, err := callMCP(t, srv, tokB, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stB, "team") {
		t.Fatalf("B 不该看到 A 的关注，实际:\n%s", stB)
	}
}

// TestRemindClear 锁定 remind - 清空。
func TestRemindClear(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "remind daily@zero"); err != nil {
		t.Fatal(err)
	}
	out, err := callMCP(t, srv, tok, "remind -")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cleared") {
		t.Fatalf("remind - 应报 cleared，实际: %s", out)
	}
	st, err := callMCP(t, srv, tok, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(st, "watched:") {
		t.Fatalf("清空后 status 不该有 watched 段，实际:\n%s", st)
	}
}
