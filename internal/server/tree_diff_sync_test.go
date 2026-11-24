package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 根治整树重发：commit 导致基准前进后，再写新内容，同步必须走
// 「树到树差分」只发增量，绝不许把整棵树重发一遍。
//
// 踩过的坑（2026-11-24，rock64 实测抓到）：
// 18,446 篇里改了 1 篇，日志打出：
//   sending full tree snapshot (18446 entries)
// 虽然对端只 applied 1，但整整 18,446 条被完整序列化、网络传输、
// 对端逐个路径比对，每轮耗时数分钟，在慢速板子上浪费 CPU 和带宽。
func TestTreeDiffSendsOnlyChangedPaths(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	down, downData := newWatchTestServer(t)
	downTok := mintRestricted(t, downData, auth.Claims{Sub: "zero", Name: "down", Cap: "rwx"})

	ts := httptest.NewServer(down.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 1. 造 10 篇文档并提交：建立一个有体积的基础库
	for i := 0; i < 10; i++ {
		writeDaily(t, up, upTok, "seed-"+string(rune('a'+i))+".md", "内容")
	}
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "base commit"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 2. 首轮同步：对端手上是空的，必然要整树快照
	res1, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("首轮快照: %v", err)
	}
	if res1.Applied < 10 {
		t.Fatalf("首轮应把全部 10 篇送过去，实际 applied=%d", res1.Applied)
	}

	// 3. 次轮同步：没有任何改动，必须 0 传输（收敛）
	res2, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("次轮同步: %v", err)
	}
	if res2.Applied != 0 {
		t.Fatalf("次轮无改动应 0 应用，实际 applied=%d", res2.Applied)
	}

	// 4. 关键验证：对端停在旧树上，本端之后又提交了两轮（中间没有推送）。
	//
	// 这是 rock64 的真实形状：每 5 分钟一轮，中间 [commit] 折叠过若干次，
	// 于是推送时「对端手上的树」与「本端当前基准」必然不同。
	// 早先遇到这种不同就无脑发整树快照——12 篇里只变了 2 篇也发 12 篇；
	// 真实库上是 18,446 篇里变 1 篇，照样发 18,446 篇。
	writeDaily(t, up, upTok, "seed-a.md", "commit 之后的第一轮修改")
	if _, err := up.commitBranch("daily", upRepo, "zero", "second commit"); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	writeDaily(t, up, upTok, "new-one.md", "第二轮新增")
	if _, err := up.commitBranch("daily", upRepo, "zero", "third commit"); err != nil {
		t.Fatalf("third commit: %v", err)
	}

	// 核心断言直接打在「发送端到底发了什么」上：
	// 必须是一批普通增量命令（几条 set/delete），而不是一条整树 Replace。
	//
	// 不能用 res.Applied 断言：seed-a.md 会被对端的冲突保护留下本地副本，
	// 那条不计入 applied——那是冲突机制在正常工作，与「发了多少」是两件事。
	peerTree := up.workSync.peerTreeOf("daily", "zero", "down")
	upLog, _ := up.opLogs.For("daily", "zero")
	if peerTree == upLog.BaseTree() {
		t.Fatalf("前提不成立：对端树与该是本端基准不同（都等于 %s），否则没进差分分支",
			shortSHA(peerTree))
	}
	batch, err := up.exportOpsFromPeer("daily", "zero", up.peerOutSeq("daily", "zero", "down"), peerTree, "down")
	if err != nil {
		t.Fatalf("导出批次: %v", err)
	}
	if isSnapshotBatch(batch) {
		t.Fatalf("对端停在旧树时不该发整树快照（%d entries），必须走树到树差分——"+
			"这就是 rock64 每轮 18,446 条的根因", len(batch.Ops[0].Batch))
	}
	if len(batch.Ops) != 2 {
		t.Fatalf("应只发 2 条增量（改 seed-a、加 new-one），实际 %d 条", len(batch.Ops))
	}

	res3, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("第三轮差分推送失败: %v", err)
	}
	if res3.Applied+len(res3.Conflicted) == 0 {
		t.Fatalf("第三轮应有内容落地，实际 applied=%d conflicted=%d", res3.Applied, len(res3.Conflicted))
	}

	// 5. 对端内容必须完全正确
	for _, name := range []string{"seed-b.md", "seed-j.md", "new-one.md"} {
		if !down.repoHas("daily", "zero", name) {
			t.Fatalf("对端缺少 %s", name)
		}
	}

	// 6. 紧接着再推一轮：必须立刻收敛为 0，不能每轮重复发这 2 篇
	res4, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("第四轮推送: %v", err)
	}
	if res4.Applied != 0 {
		t.Fatalf("差分落地后必须收敛，第四轮应 0 应用，实际 applied=%d", res4.Applied)
	}
}
