package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 下游必须能主动从上游拉取**尚未 commit** 的工作层写入。
//
// 拓扑（2026-11-22 定）：hub 常开是上游，笔记本随开随走是下游。
// 上游不会回头连接下游——笔记本 IP 会变、也经常不在线。
// 所以「拉」这一半只能由下游发起。
//
// 为什么必须有它：只有 push 的话，上游一条未提交的记忆，
// 下游永远看不见，直到上游某次 commit 之后才能靠传统 Git fetch 补上。
// 而工作层是同步主体、commit 只是书签——这条链断在 pull 上就等于
// 退回到「必须提交才能同步」。
func TestDownstreamPullsUncommittedWriteFromUpstream(t *testing.T) {
	up, upData := setupMountServer(t)
	down, _ := setupMountServer(t)
	upToken := mintRestricted(t, upData, auth.Claims{Sub: "zero", Cap: "rwx"})

	upSrv := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer upSrv.Close()

	const path = "2026-11-22/upstream-only.md"
	const body = "上游刚写下、还没有 commit 的一条记忆"

	// 上游写一条，**不 commit**。
	if _, err := callMCPRaw(t, up, upToken, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + path + " $BODY",
			"vars": map[string]string{"BODY": body},
		},
	}); err != nil {
		t.Fatalf("上游写入: %v", err)
	}

	repoDown, _ := down.projects.Get("blackegg/me")
	if _, found, _ := repoDown.Stat("zero", path); found {
		t.Fatal("下游本来不该有这条，测试前提不成立")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	res, err := down.pullWorkFromPeer(client, upSrv.URL, upToken, "up", "blackegg/me", "zero")
	if err != nil {
		t.Fatalf("下游拉取报错: %v", err)
	}
	if res.Applied == 0 {
		t.Fatalf("下游拉取后没有应用任何改动: %+v", res)
	}

	entry, found, err := repoDown.Stat("zero", path)
	if err != nil || !found {
		t.Fatalf("下游拉取后仍找不到该路径: err=%v found=%v", err, found)
	}
	got, err := repoDown.BlobBody(entry.Hash)
	if err != nil {
		t.Fatalf("读下游正文: %v", err)
	}
	if string(got) != body {
		t.Fatalf("正文不符: want %q got %q", body, string(got))
	}

	// 幂等：再拉一次不该重复应用，也不该报错。
	res2, err := down.pullWorkFromPeer(client, upSrv.URL, upToken, "up", "blackegg/me", "zero")
	if err != nil {
		t.Fatalf("第二次拉取报错: %v", err)
	}
	if res2.Applied != 0 {
		t.Fatalf("第二次拉取不该重复应用（幂等被破坏）: %+v", res2)
	}
}

// 墓碑必须跟着拉回来：上游删了，下游不能留一份自己复活它。
func TestDownstreamPullAppliesUpstreamDelete(t *testing.T) {
	up, upData := setupMountServer(t)
	down, downData := setupMountServer(t)
	upToken := mintRestricted(t, upData, auth.Claims{Sub: "zero", Cap: "rwx"})
	downToken := mintRestricted(t, downData, auth.Claims{Sub: "zero", Cap: "rwx"})

	upSrv := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer upSrv.Close()

	const path = "2026-11-22/to-be-deleted.md"

	for _, pair := range []struct {
		s   *Server
		tok string
	}{{up, upToken}, {down, downToken}} {
		if _, err := callMCPRaw(t, pair.s, pair.tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + path + " $BODY",
				"vars": map[string]string{"BODY": "两端都有"},
			},
		}); err != nil {
			t.Fatalf("写: %v", err)
		}
		if _, err := callMCP(t, pair.s, pair.tok, "commit blackegg/me@zero -m init"); err != nil {
			t.Fatalf("提交: %v", err)
		}
	}

	repoUp, _ := up.projects.Get("blackegg/me")
	if err := repoUp.Delete("zero", path); err != nil {
		t.Fatalf("上游删除: %v", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	if _, err := down.pullWorkFromPeer(client, upSrv.URL, upToken, "up", "blackegg/me", "zero"); err != nil {
		t.Fatalf("下游拉取报错: %v", err)
	}

	repoDown, _ := down.projects.Get("blackegg/me")
	if _, found, _ := repoDown.Stat("zero", path); found {
		t.Fatal("上游已删，下游拉取后却还在——离线节点会借此复活已删内容")
	}
}
