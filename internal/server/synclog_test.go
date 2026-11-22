package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 同步流水必须能被 agent 读到，且上下游两边都能看到「对方到哪了」。
//
// 这是「记忆体情况」的核心组成：不是运维日志，而是记忆体自身的状态。
// 缺了它，agent 只能回答「我这里有这些」，答不出「上游还有没有没拉过来的」。
func TestSyncJournalRecordsBothDirections(t *testing.T) {
	up, upData := setupMountServer(t)
	tokUp := mintRestricted(t, upData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 上游写两条（不 commit）。
	for _, p := range []string{"j1.md", "j2.md"} {
		if _, err := callMCPRaw(t, up, tokUp, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + p + " $BODY",
				"vars": map[string]string{"BODY": "内容 " + p},
			},
		}); err != nil {
			t.Fatalf("上游写入 %s: %v", p, err)
		}
	}

	upSrv := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer upSrv.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	down, downData := setupMountServer(t)
	tokDown := mintRestricted(t, downData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 下游拉一轮。
	if _, err := down.pullOpsFromPeer(client, upSrv.URL, tokUp, "up", "blackegg/me", "zero"); err != nil {
		t.Fatalf("拉取: %v", err)
	}

	// 一、下游的流水里必须有这一轮 pull，且带上游的最新位点。
	recs := down.syncLog.Recent(10)
	if len(recs) == 0 {
		t.Fatal("下游没有留下任何同步流水")
	}
	var pullRec *syncRecord
	for i := range recs {
		if recs[i].Dir == "pull" {
			pullRec = &recs[i]
			break
		}
	}
	if pullRec == nil {
		t.Fatalf("流水里没有 pull 记录: %+v", recs)
	}
	if pullRec.PeerHead == 0 {
		t.Fatalf("pull 记录必须带上游位点（PeerHead），实际 %+v", pullRec)
	}
	if pullRec.Applied == 0 {
		t.Fatalf("pull 记录应报告应用条数，实际 %+v", pullRec)
	}

	// 二、mem('sync') 无参必须能读出这份流水，并且提到上游位点。
	out, err := callMCP(t, down, tokDown, "sync")
	if err != nil {
		t.Fatalf("sync 无参: %v", err)
	}
	// 拓扑视图判据：下游必须能报出「上游是谁、我在哪、落后多少」。
	if !strings.Contains(out, "upstream:") {
		t.Fatalf("sync 无参应输出上游区块:\n%s", out)
	}
	if !strings.Contains(out, "blackegg/me@zero") {
		t.Fatalf("sync 无参应报出项目与分支:\n%s", out)
	}
	if !strings.Contains(out, "recent events") {
		t.Fatalf("sync 无参应输出最近事件:\n%s", out)
	}

	// 三、反向：下游推一条，上游的流水里也要有 push 记录。
	if _, err := callMCPRaw(t, down, tokDown, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:back.md $BODY",
			"vars": map[string]string{"BODY": "下游写的"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := down.pushOpsToPeer(client, upSrv.URL, tokUp, "up", "blackegg/me", "zero"); err != nil {
		t.Fatalf("推送: %v", err)
	}
	pushRecs := down.syncLog.Recent(10)
	found := false
	for _, r := range pushRecs {
		if r.Dir == "push" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("推送之后流水里应有 push 记录: %+v", pushRecs)
	}
}

// 流水必须跨重启保留：重启后仍能回答「上次同步是什么时候」。
func TestSyncJournalSurvivesRestart(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	srv.recordSync(syncRecord{
		Dir: "pull", Peer: "up", Project: "daily", Branch: "zero",
		From: 0, To: 7, PeerHead: 12, Applied: 7,
	})
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开同一 data 目录。
	srv2 := newServerAt(t, dataDir)
	t.Cleanup(func() { srv2.Close() })
	recs := srv2.syncLog.Recent(5)
	if len(recs) == 0 {
		t.Fatal("重启后流水丢了")
	}
	if recs[0].To != 7 || recs[0].PeerHead != 12 {
		t.Fatalf("重启后流水内容不对: %+v", recs[0])
	}
	out, err := callMCP(t, srv2, tok, "sync")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "peer 12") {
		t.Fatalf("重启后 sync 应报出上游位点 (peer 12):\n%s", out)
	}
}

// 上游视角：下游拉过之后，上游必须能回答「谁追到哪了」。
func TestSyncJournalUpstreamSeesDownstreamProgress(t *testing.T) {
	up, upData := setupMountServer(t)
	tokUp := mintRestricted(t, upData, auth.Claims{Sub: "zero", Cap: "rwx"})
	upTokForCall := tokUp

	for _, p := range []string{"u1.md", "u2.md", "u3.md"} {
		if _, err := callMCPRaw(t, up, tokUp, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + p + " $BODY",
				"vars": map[string]string{"BODY": "内容 " + p},
			},
		}); err != nil {
			t.Fatalf("写入 %s: %v", p, err)
		}
	}
	upSrv := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer upSrv.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	down, _ := setupMountServer(t)
	if _, err := down.pullOpsFromPeer(client, upSrv.URL, tokUp, "up", "blackegg/me", "zero"); err != nil {
		t.Fatalf("下游拉取: %v", err)
	}

	// 上游的流水里必须出现 served 记录，且带上下游的 node 名。
	found := false
	for _, r := range up.syncLog.Recent(10) {
		if r.Dir == "served" && r.Peer != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("上游应记录下游的拉取请求，实际 %+v", up.syncLog.Recent(10))
	}

	// 在上游问 mem('sync')，输出里应能看到下游已追到哪。
	out, err := callMCP(t, up, upTokForCall, "sync")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "downstreams (") {
		t.Fatalf("上游视角应报出下游列表:\n%s", out)
	}
}
