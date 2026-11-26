package server

import (
	"strings"
	"testing"
)

// 一批命令里有被拒的，水位就不能跳过它——否则那条命令永远不再被重拉，
// 内容永久缺失，而水位、状态、日志全都显示「同步成功」。
//
// 这条缺陷的形状（2026-11-26 修）：
//
//	if batch.To > 0 {
//	    s.notePeerSeq(project, branch, batch.Node, batch.To)   // 无条件推进
//	}
//
// res.Rejected 非空时，那几条命令根本没落地，水位却走到了 batch.To。
// 下一轮 pull 从 after=batch.To 开始要，被拒的那条落在窗口之外，
// 从此没有任何一轮会再请求它。verifyBatchContent 会在内容树上出声，
// 但那只在带 Rebase + ContentTree 的批次上才成立；位点增量批次
// （最常见的那种）没有 ContentTree，于是完全静默。
//
// 正确做法：推进到「最早那条被拒命令的前一位」。
func TestRejectedOpsDoNotAdvanceWatermark(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "zero"
	const peer = "peer-a"

	// 造一批：seq 1 能落地，seq 2 被拒（rename 源不存在），seq 3 能落地。
	//
	// 用 rename 制造拒绝是有意的：它不依赖脏层状态，也不会被幂等跳过，
	// 拒绝原因明确（source path does not exist locally）。
	batch := opBatch{
		Project: project, Branch: branch, Node: peer,
		From: 0, To: 3,
		Ops: []treeOp{
			{Seq: 1, Node: peer, Kind: opSet, Path: "good-1.md", Mode: "100644", SHA: ""},
			{Seq: 2, Node: peer, Kind: opRename, From: "does-not-exist.md", Path: "target.md"},
			{Seq: 3, Node: peer, Kind: opSet, Path: "good-3.md", Mode: "100644", SHA: ""},
		},
	}
	// seq 1 与 3 用空 SHA 会被 applyOp 怎么处理不影响本测试的结论：
	// 我们只断言「水位不越过被拒的那条」，所以只要 seq 2 确定被拒即可。
	res, _ := srv.ingestOps(project, branch, batch)

	if len(res.Rejected) == 0 {
		t.Fatalf("seq 2 的 rename 源不存在，必须被拒；实际 Rejected 为空（测试前提不成立）")
	}
	if len(res.RejectedSeq) != len(res.Rejected) {
		t.Fatalf("RejectedSeq 必须与 Rejected 一一对应：%d vs %d", len(res.RejectedSeq), len(res.Rejected))
	}

	got := srv.peerSeq(project, branch, peer)
	if got != 1 {
		t.Fatalf("有命令被拒时水位只能推进到最早被拒位点的前一位（期望 1），实际 %d。"+
			"水位越过被拒命令 = 那条命令永远不会被重拉，内容永久缺失", got)
	}
}

// 没有拒绝时水位照常推进到 batch.To，别把正常路径也钉住。
func TestNoRejectionAdvancesWatermarkToBatchTo(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "zero"
	const peer = "peer-b"

	batch := opBatch{
		Project: project, Branch: branch, Node: peer,
		From: 0, To: 2,
		Ops: []treeOp{
			{Seq: 1, Node: peer, Kind: opSet, Path: "a.md", Mode: "100644", SHA: ""},
			{Seq: 2, Node: peer, Kind: opSet, Path: "b.md", Mode: "100644", SHA: ""},
		},
	}
	res, _ := srv.ingestOps(project, branch, batch)
	if len(res.Rejected) != 0 {
		t.Fatalf("本批不应有拒绝，实际: %v", res.Rejected)
	}
	if got := srv.peerSeq(project, branch, peer); got != 2 {
		t.Fatalf("无拒绝时水位应推进到 batch.To=2，实际 %d", got)
	}
}

// pinWatermark 必须只降不升。
//
// 同一个批次里可能既有缺正文的命令（把水位钉到 pin-1），又有被拒的命令
// （把水位钉到 minBad-1）。两处都往回钉，但后算出的位点可能更靠后。
// 若用无条件赋值，后一处会把水位重新抬过前一处钉住的命令——
// 「永久丢失」被重新种回去，而两行日志都写着「已钉住」。
func TestPinWatermarkOnlyMovesDown(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "zero"
	const peer = "peer-c"
	key := workKey(project, branch, peer)

	forceWatermark(&srv.workSync.in, key, 10)

	if got := pinWatermark(&srv.workSync.in, key, 5); got != 5 {
		t.Fatalf("第一次钉：期望降到 5，实际 %d", got)
	}
	if got := pinWatermark(&srv.workSync.in, key, 8); got != 5 {
		t.Fatalf("第二次钉（更靠后）绝不能把水位抬高：期望仍为 5，实际 %d", got)
	}
	if got := pinWatermark(&srv.workSync.in, key, 3); got != 3 {
		t.Fatalf("更靠前的钉子应当生效：期望 3，实际 %d", got)
	}
	if got := pinWatermark(&srv.workSync.in, key, -1); got != 0 {
		t.Fatalf("负位点应归零，实际 %d", got)
	}
}

// safePeerAdvanceSeq 的边界：整树整体失败（seq 0）时整批不可信，不推进。
func TestSafePeerAdvanceSeqEdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		batchTo int64
		rej     []string
		seqs    []int64
		want    int64
	}{
		{"无拒绝", 7, nil, nil, 7},
		{"batch.To 为 0", 0, nil, nil, 0},
		{"拒绝在 seq 3", 7, []string{"x"}, []int64{3}, 2},
		{"拒绝在 seq 1", 7, []string{"x"}, []int64{1}, 0},
		{"多个拒绝取最早", 7, []string{"x", "y"}, []int64{5, 2}, 1},
		{"整树失败(seq 0)不推进", 7, []string{"x"}, []int64{0}, 0},
		{"有拒绝但没带位点", 7, []string{"x"}, nil, 0},
	}
	for _, c := range cases {
		if got := safePeerAdvanceSeq(c.batchTo, c.rej, c.seqs); got != c.want {
			t.Errorf("%s: 期望 %d，实际 %d", c.name, c.want, got)
		}
	}
}

// 被拒时不能把本地命令流基准记成「已与对端对齐」。
//
// 记了基准，下一轮发送端就按「对端已经在这棵树上」算差分，
// 而被拒那条的差异根本不在算出来的差分里——分歧被封存。
func TestRejectedOpsDoNotRecordPeerBase(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "zero"
	const peer = "peer-d"

	batch := opBatch{
		Project: project, Branch: branch, Node: peer,
		From: 0, To: 1,
		Ops: []treeOp{
			{Seq: 1, Node: peer, Kind: opRename, From: "nope.md", Path: "target.md"},
		},
	}
	res, _ := srv.ingestOps(project, branch, batch)
	if len(res.Rejected) == 0 {
		t.Fatal("rename 源不存在必须被拒，测试前提不成立")
	}
	if got := srv.peerSeq(project, branch, peer); got != 0 {
		t.Fatalf("被拒时水位必须停在 0，实际 %d", got)
	}
	if base := srv.workSync.baseOf(project, branch, peer); base != "" {
		t.Fatalf("被拒时不得记对端基准，实际记了 %q（分歧会被封存）", base)
	}
}

// 拒收的日志必须点名——发送方看到 "applied N" 会以为同步成功。
func TestRejectedReasonIsStructured(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "zero"
	batch := opBatch{
		Project: project, Branch: branch, Node: "p",
		From: 0, To: 1,
		Ops: []treeOp{{Seq: 1, Node: "p", Kind: opRename, From: "gone.md", Path: "t.md"}},
	}
	res, _ := srv.ingestOps(project, branch, batch)
	if len(res.Rejected) != 1 {
		t.Fatalf("期望 1 条拒绝，实际 %v", res.Rejected)
	}
	if !strings.Contains(res.Rejected[0], "source path does not exist") {
		t.Fatalf("拒绝原因应说明是源路径不存在，实际: %s", res.Rejected[0])
	}
	if len(res.RejectedSeq) != 1 || res.RejectedSeq[0] != 1 {
		t.Fatalf("拒绝位点必须结构化带出来（期望 [1]），实际 %v", res.RejectedSeq)
	}
}
