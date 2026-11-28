package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
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

// 快照成功之后，下一轮必须能发增量而不是报 base tree mismatch。
//
// 这一条覆盖的是「记账」那一环：快照落地后对端手上的树是内容树，
// 如果发送端还是按自己的 oplog 原点声明增量，接收端按内容树校验，
// 两边永远对不上——实测症状是每轮 500:
//
//	op-sync: base tree mismatch (local <内容树>, peer <原点>)
func TestSnapshotThenDeltaKeepsAligning(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	down, downData := newWatchTestServer(t)
	downTok := mintRestricted(t, downData, auth.Claims{Sub: "zero", Name: "down", Cap: "rwx"})

	ts := httptest.NewServer(down.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 先写并提交 → 命令流归零、基准前进
	writeDaily(t, up, upTok, "a.md", "A")
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 提交之后再写一条**不提交**：这一步是关键。
	//
	// 不写它的话，快照的内容树恰好等于 oplog 原点（工作层与提交一致），
	// 「对端基准记成内容树」和「记成原点」两种写法算出同一个值，
	// 测试就抓不住记账错误——实测确认过它照样通过。
	writeDaily(t, up, upTok, "a2.md", "A2")
	if _, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero"); err != nil {
		t.Fatalf("首轮快照: %v", err)
	}

	// 之后连续写两轮、推两轮：每一轮都必须是增量，且不能报基准不匹配
	for i, name := range []string{"b.md", "c.md"} {
		writeDaily(t, up, upTok, name, name)
		res, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
		if err != nil {
			t.Fatalf("第 %d 轮增量推送失败（基准记账错了就会这样）: %v", i+2, err)
		}
		if res.Applied != 1 {
			t.Fatalf("第 %d 轮应只同步 1 篇，实际 applied=%d —— 快照成功后的记账没跟上", i+2, res.Applied)
		}
	}
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if !down.repoHas("daily", "zero", name) {
			t.Fatalf("对端缺少 %s", name)
		}
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
		"name": "mem",
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
	// 让本端记下「对端手上有一棵非空的树」
	if s := up.workSync; s != nil {
		s.notePeerTree("daily", "zero", "down", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	}

	// 本端树是空的，对端基准非空。
	//
	// 早先这里是**报错拒绝**。错在两处：① 它把「算不出来」和「真的是空的」
	// 混为一谈（currentContentTree 早先两种都返回 ""）；② 即便是真的空了，
	// 永久报错也会把这条分支的同步彻底卡死——实测删掉分支上唯一一条内容后，
	// 对端每 3 秒重试一次、每次都 500，此后新写的内容也到不了对端。
	//
	// 现在改成：不发整树快照（快照的语义是「照它重建」，会抹掉对端独有的
	// 内容），只发命令流。对端独有的内容按「拉取不推断删除」留在原处。
	//
	// 这里本端是用 repo.Delete 直接删的，**没有 opDelete 命令**，
	// 所以命令流里没有删除——对端应该一条都不丢。
	if _, perr := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero"); perr != nil {
		t.Fatalf("空树不该再报错（那会把这条分支的同步永久卡死）：%v", perr)
	}

	// 安全性质：对端自己的内容必须还在。
	if !down.repoHas("daily", "zero", "keep.md") {
		t.Fatalf("对端自己的内容被空树快照清掉了")
	}
	// 而且对端那份 seed.md 也不许被推断删除——本端没发过 opDelete。
	if !down.repoHas("daily", "zero", "seed.md") {
		t.Fatalf("对端内容被推断成删除（拉取只加只改，绝不按「对端没有」推断删除）")
	}
}

// 显式删除必须**沿命令流传到对端**——这是「删除只认显式指令」的另一半：
// 不显式删的不许丢，显式删的要真的删掉。
//
// 自狗粮抓到的形状：A 删掉分支上唯一一条内容后，导出侧被空树安全阀挡成
// 永久 500，对端每 3 秒重试一次全失败——删除既没传过去，之后新写的内容
// 也一起堵死在这条分支上。
func TestExplicitDeleteTravelsOnTheCommandStream(t *testing.T) {
	up, _ := newWatchTestServer(t)
	down, downData := newWatchTestServer(t)
	downTok := mintRestricted(t, downData, auth.Claims{Sub: "zero", Name: "down", Cap: "rwx"})

	ts := httptest.NewServer(down.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 本端先有两条，推给对端。
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if err := upRepo.Write("zero", "gone.md", []byte("要被显式删掉的"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := upRepo.Write("zero", "stay.md", []byte("留下的"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero"); err != nil {
		t.Fatal(err)
	}
	if !down.repoHas("daily", "zero", "gone.md") {
		t.Fatal("首次推送没到对端")
	}

	// 走 delete 动词：这会记一条 opDelete 进命令流。
	upRepo.Delete("zero", "gone.md")
	if _, err := up.recordTreeOp("daily", "zero", opDelete, "gone.md", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero"); err != nil {
		t.Fatalf("显式删除的推送不该失败：%v", err)
	}

	if down.repoHas("daily", "zero", "gone.md") {
		t.Fatal("显式删除没有传到对端——对端会永远留着一份本端已经删掉的内容")
	}
	if !down.repoHas("daily", "zero", "stay.md") {
		t.Fatal("显式删除只该删掉被点名的那条，别的都不许动")
	}
}
