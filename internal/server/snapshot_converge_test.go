package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 整树快照发过一次就必须收敛，不许每轮重发。
//
// 实测（2026-11-23，rock64）：快照判据写成「对端水位为 0」，而命令流
// 提交即归零、归零后 last 也是 0——快照发完之后水位仍然停在 0。
// 于是每轮同步都重发整棵树：日志里每 3 分钟一条
// 「appl 13627 / appl 13527 / appl 6532」。
//
// 判据必须是「对端记得的基准与本端不同」：基准是这段流的坐标原点，
// 位点只是流内的偏移，拿偏移判断「要不要从头发」必然弄错。
func TestSnapshotSentOnceThenConverges(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	down, downData := newWatchTestServer(t)
	downTok := mintRestricted(t, downData, auth.Claims{Sub: "zero", Name: "down", Cap: "rwx"})

	ts := httptest.NewServer(down.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 上游：写三条并提交 → 命令流归零，三条内容只在基准树里
	for _, n := range []string{"a", "b", "c"} {
		writeDaily(t, up, upTok, n+".md", n)
	}
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 第一轮：对端基准未知（""）≠ 本端基准 → 必须发整树快照
	res1, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("首轮推送: %v", err)
	}
	if res1.Applied == 0 {
		t.Fatalf("首轮应当把基准树里的内容送到对端，实际 applied=0")
	}
	for _, n := range []string{"a", "b", "c"} {
		if !down.repoHas("daily", "zero", n+".md") {
			t.Fatalf("首轮之后对端缺少 %s.md", n)
		}
	}

	// 第二轮：本端没有任何新写入、基准也没变 → 一个字节都不该发
	res2, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("第二轮推送: %v", err)
	}
	if res2.Applied != 0 || res2.Deleted != 0 {
		t.Fatalf("已经对齐之后不该再有内容流动，实际 applied=%d deleted=%d —— "+
			"这是「判据用位点而不用基准」的典型症状：快照每轮重发", res2.Applied, res2.Deleted)
	}

	// 第三轮：本端写一条新的 → 只该发这一条增量，不该整树重发
	writeDaily(t, up, upTok, "d.md", "D")
	res3, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("第三轮推送: %v", err)
	}
	if res3.Applied != 1 {
		t.Fatalf("只写了一篇新内容，应当只同步这一篇，实际 applied=%d", res3.Applied)
	}
	if !down.repoHas("daily", "zero", "d.md") {
		t.Fatalf("增量没到对端")
	}
}

// 空树快照会把对端墓碑成空。对着还有内容的对端刷一棵空树 = 误删。
func TestRefusesToWipePeerWithEmptySnapshot(t *testing.T) {
	up, _ := newWatchTestServer(t)

	down, downData := newWatchTestServer(t)
	downTok := mintRestricted(t, downData, auth.Claims{Sub: "zero", Name: "down", Cap: "rwx"})

	ts := httptest.NewServer(down.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 对端先有内容（由它自己写，不经过同步）
	if _, err := callMCPRaw(t, down, downTok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write daily@zero:keep.md $BODY",
			"vars": map[string]string{"BODY": "对端自己的内容"},
		},
	}); err != nil {
		t.Fatalf("对端写入: %v", err)
	}

	// 把本端那份唯一的 seed.md 删掉，让树真正变空
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if err := upRepo.Delete("zero", "seed.md"); err != nil {
		t.Fatal(err)
	}
	// 让本端记下「对端基准是 X（非空）」
	if s := up.workSync; s != nil {
		s.noteBase("daily", "zero", "down", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	}

	// 本端树是空的，对端基准非空 → 必须拒绝，不许把它清空
	_, perr := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if perr == nil {
		t.Fatalf("对着非空对端推空树快照必须报错拒绝")
	}

	// 对端内容必须还在
	if !down.repoHas("daily", "zero", "keep.md") {
		t.Fatalf("对端自己的内容被空树快照清掉了")
	}
}
