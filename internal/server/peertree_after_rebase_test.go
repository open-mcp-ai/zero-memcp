package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 拉过一次带 Rebase 的批次之后，必须记下「对端手上是哪棵树」。
//
// 实测来源（2026-11-25）：
//
//	把 hub-3060 从 Git 远端改成工作层对端后，第一次同步推的是
//	**18872 条路径的全量快照**——因为推送侧不知道对端有什么，只能按
//	「对端是空的」处理。3060 开着索引，于是逐条 refreshPath 抢分支写令牌，
//	一个索引重建把令牌占满，满屏「分支写令牌等待超时（1m0s）」，
//	推送侧 5 分钟等不到字节、直接超时。
//
//	而那一刻两边的**内容其实完全一样**。本该一条命令都不发。
//
// 根因：拉取的响应里没有「对端当前内容树」这个字段（BaseTree 在各分支里
// 含义不同，不能当它用）。但有一处语义是确定的：带 Rebase 的批次落地后，
// 本端内容等于发送端内容。那一刻记下来，推送侧下一轮就能算差分。
func TestPullRecordsPeerTreeAfterRebase(t *testing.T) {
	src, srcData := setupMountServer(t) // 提供内容的对端
	dst, dstData := setupMountServer(t) // 拉取方
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	srcSrv := httptest.NewServer(src.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srcSrv.Close()

	const project, branch = "blackegg/me", "zero"

	// 对端先有内容（对拉取方来说，这批必然带 Rebase：
	// 它没有任何基准，只能收到整树/差分快照）。
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write " + project + "@" + branch + ":a.md $BODY",
			"vars": map[string]string{"BODY": "对端的内容"},
		},
	}); err != nil {
		t.Fatalf("对端写入: %v", err)
	}

	// 先给拉取方一个「非空但不对」的基准记忆：这样它报出去的基准确实存在，
	// 对端认不出这棵树，才会走 Rebase（整树/树到树差分）那条路。
	// 不给的话，请求里 baseTree 为空，对端会按「按位点补」发增量——
	// 那条路不重设基准，也就没有「对端内容树」可记。
	// 必须在本地对象库里真实存在，否则导出器算不出差分只能整树。
	if _, err := callMCPRaw(t, dst, dstToken, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write " + project + "@" + branch + ":mine.md $BODY",
			"vars": map[string]string{"BODY": "本端自己的内容"},
		},
	}); err != nil {
		t.Fatalf("本端写入: %v", err)
	}
	// 必须先封箱：未提交的本地改动在整树 Replace 落地时会被保留，
	// 那样两边内容就不相等了，也就无法验证「记下的树 = 对端内容」。
	if _, err := callMCP(t, dst, dstToken, "commit "+project+"@"+branch); err != nil {
		t.Fatalf("封箱: %v", err)
	}
	bogus := dst.currentContentTree(project, branch)
	dst.workSync.notePeerTree(project, branch, dst.resolvePeer("src"), bogus)

	client := &http.Client{Timeout: 10 * time.Second}
	if _, err := dst.pullOpsFromPeer(client, srcSrv.URL, srcToken, "src", project, branch); err != nil {
		t.Fatalf("拉取: %v", err)
	}

	// 关键断言：拉完之后，必须知道对端手上是哪棵树。
	peerKey := dst.resolvePeer("src")
	tree := dst.workSync.peerTreeOf(project, branch, peerKey)
	if tree == "" {
		t.Fatal("拉过一次 Rebase 批次后，必须记下对端手上的内容树；" +
			"不记的话推送侧下一轮会按「对端是空的」发全量快照")
	}

	// 而且这棵树必须等于本端现在的内容树（Rebase 的语义就是两边内容相同）。
	mine := dst.currentContentTree(project, branch)
	if tree != mine {
		t.Fatalf("记下的对端树应当等于本端内容树\n  对端=%s\n  本端=%s", tree, mine)
	}
	_ = dstToken
}
