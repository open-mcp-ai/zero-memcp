package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 导出这一侧不许做那件 O(条数) 的事。
//
// 起因（2026-11-28）：流水里出现「0 条变化却耗时 37 秒」的拉取——
// `pull weft@main.record → hub-3060 applied=0 dur=37.4s`。
//
// 那条路径上有三处 O(N)：
//  1. currentContentTree —— 摊平 + 重写树（已加缓存）
//  2. 为了算差分先 flattenTreeToOps —— 又摊平一次
//  3. 再把摊平结果重写成一棵树 —— 与 1 完全重复
//
// 2 和 3 是纯重复：flattenTreeToOps 就是 FlattenBranch 的一比一映射，
// 不筛任何东西，所以它写出来的正是 1 刚算过的那棵树。
//
// 这里钉的是**事实**而不是耗时：两棵树相等时，一次摊平都不该发生。
// 用时间断言会在慢机器上假红，而「有没有做这件事」是可断言的。
func TestEqualContentTreesSkipFlatteningEntirely(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	// 造两次提交：oldTree 是更早的内容，newTree 是当前内容。
	//
	// 为什么需要两个：commit 之后命令流原点恰好等于内容树，
	// exportOpsFromPeer 会从「base_tree == 原点」那条位点增量分支提前返回，
	// 根本走不到内容树判等。那样测出来的绿是假绿——它压根没测这件事。
	// 所以让 base_tree 是更早的树，have_tree 才是当前内容树。
	for _, p := range []string{"a.md", "b.md"} {
		if err := repo.Write("zero", p, []byte("内容 "+p), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}
	oldTree, _ := srv.currentContentTree("blackegg/me", "zero")
	if err := repo.Write("zero", "c.md", []byte("内容 c.md"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m more"); err != nil {
		t.Fatal(err)
	}

	mine, _ := srv.currentContentTree("blackegg/me", "zero")
	if mine == "" || mine == oldTree {
		t.Fatalf("两次提交后内容树必须变化：old=%q now=%q", oldTree, mine)
	}

	// 对端报的 have_tree 与本端内容树相同 = 「对端手上就是这份内容」。
	// 这是提交之后最常见的一轮：本端基准前进了一次，内容一个字节没变。
	// base_tree 故意给更早的树，好让代码真的走到内容树判等那一步。
	before := srv.flattenCalls.Load()
	batch, err := srv.exportOpsFromPeer("blackegg/me", "zero", 0, oldTree, mine, "peer-x")
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.flattenCalls.Load() - before; got != 0 {
		t.Fatalf("两边内容相同时不该摊平，实际摊平了 %d 次（这正是那 37 秒的来源）", got)
	}
	if len(batch.Ops) != 0 {
		t.Fatalf("两边内容相同时不该发命令，实际发了 %d 条", len(batch.Ops))
	}
	if batch.ContentTree != mine {
		t.Fatalf("内容树必须报回去（对端靠它算差分），实际 %q", batch.ContentTree)
	}
}

// 内容不同时要走树到树差分，也不该摊平。
//
// 差分只需要两棵树，而两棵树都已经有了。
func TestTreeDiffAlsoSkipsFlattening(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.md", "b.md"} {
		if err := repo.Write("zero", p, []byte("旧 "+p), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}
	old, _ := srv.currentContentTree("blackegg/me", "zero")

	// 改一条并提交：对端手里是 old，本端现在是新的。
	if err := repo.Write("zero", "a.md", []byte("新 a.md"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m change"); err != nil {
		t.Fatal(err)
	}
	if now, _ := srv.currentContentTree("blackegg/me", "zero"); now == old {
		t.Fatal("改了内容但内容树没变")
	}

	before := srv.flattenCalls.Load()
	batch, err := srv.exportOpsFromPeer("blackegg/me", "zero", 0, old, old, "peer-x")
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.flattenCalls.Load() - before; got != 0 {
		t.Fatalf("树到树差分不该摊平，实际摊平了 %d 次", got)
	}
	if len(batch.Ops) == 0 {
		t.Fatal("内容不同时必须发命令")
	}
	if !batch.Rebase {
		t.Fatal("差分批次必须带 Rebase（接收端按基准树理解这批命令）")
	}
	// 差分的意义就是「只发变化的路径」：两条路径里只改了一条。
	if n := countBatchOps(batch.Ops); n != 1 {
		t.Fatalf("只改了一条却发了 %d 条命令（差分退化成整树了）", n)
	}
}

// 对端是空树时必须走整树快照——这条路上摊平是应该发生的。
//
// 反过来钉：如果为了「省掉摊平」把整树快照也弄坏，这个测试会红。
func TestEmptyPeerStillSendsFullSnapshot(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.md", "b.md"} {
		if err := repo.Write("zero", p, []byte("内容 "+p), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}

	// 真实客户端对「空分支」报的是空树 SHA，不是空串
	// （pullOpsFromPeer 里 baseTree := l.BaseTree()，而 BaseTree()
	// 把空值归一成 EmptyTreeSHA）。空串只是 opLogs 没初始化时才可能出现，
	// 拿它当输入等于测一个永远不会发生的状态。
	batch, err := srv.exportOpsFromPeer("blackegg/me", "zero", 0, gitstore.EmptyTreeSHA, "", "peer-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Ops) == 0 {
		t.Fatal("对端是空的，必须给整树快照")
	}
	// 清单要覆盖全部路径（提交可能顺带落下别的条目，所以按「包含」判，
	// 而不是按条数——按条数断言会在无关的元数据条目出现时假红）。
	for _, want := range []string{"a.md", "b.md"} {
		found := false
		for _, p := range batch.ContentPaths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("整树快照的路径清单里缺 %s，实际：%v", want, batch.ContentPaths)
		}
	}
	if batch.ContentTree == "" {
		t.Fatal("整树快照也必须报内容树")
	}
}

// 对端报了本端不认得的内容树时，不许直接退化成整树快照。
//
// 起因（2026-11-28 实测，v0.2.18 三端）：`weft@main.record` 的一次拉取
// 耗时 22.6 秒，日志形状是
//
//	applying 21332 op(s) (this can take a while)
//	TREE REPLACE ... kept 142 locally-present path(s)
//	semantic: incremental: 180 changed/new
//	index snapshot saved: 28612 docs, 70.3 MB
//
// 21,332 条路径的整树快照里，只有 23 条是对端真的没有的。
//
// 根因：差分的起点首选 `have_tree`——对端**工作层**的内容树，含它刚写完
// 还没推给本端的东西。对端每写一笔它就变一次，本端对象库里永远不会有
// 那棵树，于是 Diff 必然失败、必然退整树快照。
//
// 而那条路上还有一棵树是双方都认得的：`base_tree`（对端的命令流原点，
// 也就是它上一次提交的内容，而那批内容正是它推给本端的）。退到它算差分，
// 只发真的变化的那些路径。
func TestUnknownPeerTreeFallsBackToBaseTreeNotFullSnapshot(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	// 两次提交：让 base_tree（对端上次提交）与本端命令流原点不同，
	// 否则请求会从「base_tree == 原点」那条位点增量捷径返回，走不到差分。
	for _, p := range []string{"a.md", "b.md"} {
		if err := repo.Write("zero", p, []byte("内容 "+p), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}
	baseTree, _ := srv.currentContentTree("blackegg/me", "zero")

	// 本端又提交了一条（这就是对端将要看到的差异）。
	if err := repo.Write("zero", "c.md", []byte("内容 c"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m more"); err != nil {
		t.Fatal(err)
	}
	cur, _ := srv.currentContentTree("blackegg/me", "zero")
	if cur == baseTree {
		t.Fatal("两次提交后内容树必须不同")
	}
	// 造一棵本端对象库里不存在的树：真实场景里它是对端脏层算出来的树，
	// 对本端而言与这个假 SHA 同性质——查不到对象。
	unknown := strings.Repeat("ab", 20)

	before := srv.flattenCalls.Load()
	batch, err := srv.exportOpsFromPeer("blackegg/me", "zero", 0, baseTree, unknown, "peer-x")
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.flattenCalls.Load() - before; got != 0 {
		t.Fatalf("不该摊平（整树快照才需要摊平），实际摊平 %d 次", got)
	}
	if len(batch.ContentPaths) != 0 {
		t.Fatalf("不该发整树快照（ContentPaths 有 %d 条）", len(batch.ContentPaths))
	}
	if batch.BaseTree != baseTree {
		t.Fatalf("差分起点必须是对端的命令流原点 %q，实际 %q", baseTree, batch.BaseTree)
	}
	if !batch.Rebase {
		t.Fatal("差分批次必须带 Rebase")
	}
	// 差分里应当只有「本端有、对端没有」的那一条（c.md）。
	// 对端工作层里那些本端还没收到的内容不在其中——那是它自己的事，
	// 它会在推送时带给本端。
	if n := countBatchOps(batch.Ops); n != 1 {
		t.Fatalf("从对端命令流原点差出来的应该只有 1 条变化，实际 %d 条", n)
	}
}

// 两棵树本端都不认得时，才允许退成整树快照。
func TestBothTreesUnknownFallsBackToFullSnapshot(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "a.md", []byte("内容 a"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}
	unknown1 := strings.Repeat("ab", 20)
	unknown2 := strings.Repeat("cd", 20)
	batch, err := srv.exportOpsFromPeer("blackegg/me", "zero", 0, unknown1, unknown2, "peer-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.ContentPaths) == 0 {
		t.Fatal("两棵树都不认得时，只能整树快照——ContentPaths 必须有内容")
	}
}
