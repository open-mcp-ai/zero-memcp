package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 内容哈希比对是同步的正确性底线：基准、水位、位点都只是效率机制，
// 「应用完之后两边是不是同一棵树」才决定对错。
//
// 背景（2026-11-25，主人提出的方案）：命令流直接作用于工作树，落地后
// 比对内容哈希，对不上就走差分补齐。它取代先前「给基准加代次 gen」的
// 思路——gen 只能预防基准撞车，而内容比对所有失效一视同仁：丢命令、
// 命令被拒、正文缺失、水位记错，最后都会在这里现形。
//
// 这个测试做三件事：
//  1. 正常批次：发送端声明的 ContentTree 必须等于接收端落地后的内容树；
//  2. 篡改批次：从整树快照里抽走一条（模拟「中间丢了一条」），必须报错，
//     不许当成同步成功；
//  3. 水位不被污染：比对失败时不得记下「对端手上是这棵树」。
func TestContentTreeDetectsDroppedCommand(t *testing.T) {
	src, srcData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dst, dstData := setupMountServer(t)
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	const project, branch = "blackegg/me", "zero"

	// 源端造两条记忆并封箱：这样导出的是一棵确定的内容树。
	for _, p := range []string{"a.md", "b.md"} {
		if _, err := callMCPRaw(t, src, srcToken, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write " + project + "@" + branch + ":" + p + " $BODY",
				"vars": map[string]string{"BODY": "内容 " + p},
			},
		}); err != nil {
			t.Fatalf("源端写入 %s: %v", p, err)
		}
	}
	if _, err := callMCP(t, src, srcToken, "commit "+project+"@"+branch); err != nil {
		t.Fatalf("源端封箱: %v", err)
	}

	// 源端按「对端是空树」导出：得到带 Rebase + ContentTree 的整树快照。
	batch, err := src.exportOpsFromPeer(project, branch, 0, gitstore.EmptyTreeSHA, "dst")
	if err != nil {
		t.Fatalf("导出: %v", err)
	}
	if batch.ContentTree == "" {
		t.Fatal("Rebase 批次必须声明 ContentTree，否则接收端无从校验")
	}
	if !batch.Rebase || len(batch.Ops) != 1 || batch.Ops[0].Kind != opTree {
		t.Fatalf("预期整树快照，实际 %+v", batch)
	}

	// 把 blob 搬进接收端的对象库（真实同步里这是「先 tree 后 blob」那一趟）。
	copyBatchBlobs(t, src, dst, project, batch)

	// ① 正常落地：接收端应用后内容必须等于声明的树。
	res, err := dst.ingestOps(project, branch, batch)
	if err != nil {
		t.Fatalf("正常批次不应报错: %v", err)
	}
	if len(res.Missing) != 0 {
		t.Fatalf("本实例对象库同源，不应缺正文: %v", res.Missing)
	}
	if got := dst.currentContentTree(project, branch); got != batch.ContentTree {
		t.Fatalf("正常落地后内容应等于声明的树\n  声明=%s\n  实际=%s", batch.ContentTree, got)
	}

	// ② 换一个接收端，抽走快照里的一条 —— 模拟「中间丢了一条命令」。
	dst2, _ := setupMountServer(t)
	copyBatchBlobs(t, src, dst2, project, batch)
	broken := batch
	broken.Ops = append([]treeOp(nil), batch.Ops...)
	broken.Ops[0].Batch = append([]opEntry(nil), batch.Ops[0].Batch...)
	if len(broken.Ops[0].Batch) < 2 {
		t.Fatalf("快照至少需要两条才能做丢弃实验，实际 %d", len(broken.Ops[0].Batch))
	}
	dropped := broken.Ops[0].Batch[0].Path
	broken.Ops[0].Batch = broken.Ops[0].Batch[1:]

	_, err = dst2.ingestOps(project, branch, broken)
	if err == nil {
		t.Fatalf("抽走 %s 之后内容已经与声明不符，却被当成同步成功——这正是静默丢内容", dropped)
	}
	if !strings.Contains(err.Error(), "content tree mismatch") {
		t.Fatalf("报错应当点名内容树不一致，实际: %v", err)
	}
	t.Logf("丢弃一条被检出：%v", err)

	// ③ 水位不许被污染：比对失败的批次不能留下「对端已有这棵树」的记忆。
	if got := dst2.workSync.peerTreeOf(project, branch, dst2.resolvePeer("src")); got != "" {
		t.Fatalf("比对失败却仍记下对端树 %s，分歧会被封存", got)
	}
	_ = dstToken
}

// 有命令被拒时，水位也不能照样推进——否则那条命令永远不会重试。
//
// 这里直接钉住 verifyBatchContent 的跳过条件：Rejected 不在跳过之列。
// 缺正文（Missing）与保留本地改动（Kept/Conflicted）才允许跳过。
func TestVerifyBatchContentSkipConditions(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "zero"

	batch := opBatch{Project: project, Branch: branch, Node: "peer", Rebase: true, ContentTree: "abc"}

	// 带 Missing：合法跳过（正文还没到，树必然不完整）。
	if err := srv.verifyBatchContent(project, branch, batch, opApplyResult{Missing: []string{"sha"}}); err != nil {
		t.Fatalf("缺正文时应跳过校验，实际报错 %v", err)
	}
	// 非 Rebase 批次：不校验。
	plain := batch
	plain.Rebase = false
	if err := srv.verifyBatchContent(project, branch, plain, opApplyResult{}); err != nil {
		t.Fatalf("非 Rebase 批次不应校验，实际 %v", err)
	}
	// 有 Rejected 且内容对不上：必须报错。
	if err := srv.verifyBatchContent(project, branch, batch, opApplyResult{Rejected: []string{"x"}}); err == nil {
		t.Fatal("有命令被拒且内容不符，必须报错")
	}
}

// copyBatchBlobs 把批次里引用到的 blob 从源端对象库搬进接收端，
// 模拟同步协议里「先定终局、再按终局取正文」的那一趟。
func copyBatchBlobs(t *testing.T, src, dst *Server, project string, batch opBatch) {
	t.Helper()
	srepo, err := src.projects.Get(project)
	if err != nil {
		t.Fatalf("源端仓库: %v", err)
	}
	drepo, err := dst.projects.Get(project)
	if err != nil {
		t.Fatalf("接收端仓库: %v", err)
	}
	for _, op := range batch.Ops {
		for _, e := range op.Batch {
			body, rerr := srepo.BlobBody(e.SHA)
			if rerr != nil {
				t.Fatalf("源端读 blob %s: %v", e.SHA, rerr)
			}
			if werr := drepo.PutBlob(e.SHA, body); werr != nil {
				t.Fatalf("接收端写 blob %s: %v", e.SHA, werr)
			}
		}
	}
}
