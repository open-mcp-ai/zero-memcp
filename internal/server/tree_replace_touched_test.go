package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 整树快照刷索引只能刷**真正落地**的路径，不能刷批次里列出的每一条。
//
// 实测来源（2026-11-25，主人在问「tree 过去后再同步 blob，对面已有的
// blob 可以不用传」时把这条连带揪出来的）：
//
//	3060 有 26138 条，笔记本 25309 条，真实差异只有 829 条。
//	但整树快照把 26145 条条目全列进 ops，接收端按条目逐条 refreshPath，
//	每条都要抢一次分支写令牌，一个索引重建把令牌占满 → 同步超时。
//
// 协议本身是对的：opTree 只带 path/sha/mode，不搬正文；接收端对本地
// 已有的 blob 直接应用，只把缺的报进 Missing 让对端补。慢的不是传输，
// 是「按条目刷索引」。
func TestTreeReplaceRefreshesOnlyChangedPaths(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	const project, branch = "blackegg/me", "zero"

	// 先让两边有**完全相同**的一批内容：dst 自己写同样的路径、同样的正文。
	// 内容寻址保证 sha 也相同，于是后面的整树快照里这些条目不构成变更。
	paths := map[string]string{
		"same-1.md": "identical content 1",
		"same-2.md": "identical content 2",
		"same-3.md": "identical content 3",
	}
	for p, body := range paths {
		if _, err := callMCPRaw(t, src, srcToken, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write " + project + "@" + branch + ":" + p + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("src 写 %s: %v", p, err)
		}
		if _, err := callMCPRaw(t, dst, dstToken, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write " + project + "@" + branch + ":" + p + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("dst 写 %s: %v", p, err)
		}
	}
	// 两边都封箱：落进干净层，applyOp 才会走「已与本地一致 → 跳过」。
	for _, s := range []*Server{src, dst} {
		tok := srcToken
		if s == dst {
			tok = dstToken
		}
		if _, err := callMCP(t, s, tok, "commit "+project+"@"+branch); err != nil {
			t.Fatalf("封箱: %v", err)
		}
	}

	// src 再加一条 dst 没有的：真实变更只有这 1 条。
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write " + project + "@" + branch + ":only-on-src.md $BODY",
			"vars": map[string]string{"BODY": "only here"},
		},
	}); err != nil {
		t.Fatalf("src 加写: %v", err)
	}

	// src 导出整树快照：dst 报「我手上是空树」。空字符串会走位点增量，
	// 必须显式报空树（4b825dc6）才会走 Replace 整树这条路。
	batch, err := src.exportOpsFromPeer(project, branch, 0, "", gitstore.EmptyTreeSHA, "dst")
	if err != nil {
		t.Fatalf("导出: %v", err)
	}
	if !isSnapshotBatch(batch) {
		t.Fatalf("预期整树快照，实际 %d op(s) rebase=%v", len(batch.Ops), batch.Rebase)
	}
	entries := len(batch.Ops[0].Batch)
	if entries < 4 {
		t.Fatalf("整树快照应含 4 条条目，实际 %d", entries)
	}
	// 正文都已在 dst 的对象库里（两边内容同源），不需要补 blob。
	copyBatchBlobs(t, src, dst, project, batch)

	res, err := dst.ingestOps(project, branch, batch)
	if err != nil {
		t.Fatalf("落地: %v", err)
	}
	if len(res.Missing) != 0 {
		t.Fatalf("不应缺正文: %v", res.Missing)
	}

	// 核心断言：批次有 4 条条目，但真正落地的只有 1 条。
	if res.Applied != 1 {
		t.Fatalf("只有 1 条是真实变更，Applied 应为 1，实际 %d", res.Applied)
	}
	if len(res.Touched) != 1 {
		t.Fatalf("刷索引的路径数应等于真实变更数 1，实际 %d 条：%v\n"+
			"按批次条目刷（%d 条）就会给每条路径都重建一次索引，每条抢一次分支写令牌——"+
			"这正是 26145 条整树同步超时的原因", len(res.Touched), res.Touched, entries)
	}
	if res.Touched[0] != "only-on-src.md" {
		t.Fatalf("落地路径应是 only-on-src.md，实际 %v", res.Touched)
	}

	// 真正决定成本的是「刷索引用哪组路径」，不是 res.Touched 本身。
	// 批次列出 4 条，刷索引必须只用 1 条。
	refresh := dst.refreshPathsFor(project, branch, batch.Ops, res)
	if len(refresh) != 1 {
		t.Fatalf("刷索引应当只用 1 条真实变更路径，实际 %d 条：%v\n"+
			"（按批次条目刷会得到 %d 条，每条抢一次分支写令牌）",
			len(refresh), refresh, len(changedPathsOf(batch.Ops)))
	}
	if got := len(changedPathsOf(batch.Ops)); got != entries {
		t.Fatalf("对照：按批次条目刷应当是 %d 条，实际 %d 条（测试前提变了）", entries, got)
	}
}
