package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 提交锚点：把「树上某一刻的快照」与「命令流的位点」对应起来。
//
// 没有这张表，「先传提交、再传该提交之后的命令流」就走不通：接收端手里
// 有一个提交，却不知道该从哪一位开始要，只能让对端把整棵树重发一遍。
//
// 这些测试钉住四件事：
//
//   - 提交成功必须留下锚点，且位点取在**提交之前**（取之后会漏命令）
//   - 拿锚点提交来拉，只得到该提交之后的命令，不是整树
//   - 拿不认识的提交来拉，退化为整树而不是报错
//   - 锚点必须跨重启存活

func writeDaily(t *testing.T, srv *Server, tok, path, body string) {
	t.Helper()
	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  fmt.Sprintf("write daily@zero:%s $BODY", path),
			"vars": map[string]string{"BODY": body},
		},
	}); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// 提交必须留下锚点，且锚点位点是提交**开始之前**的那一位。
//
// 方向为什么重要：若记成提交之后的位点，提交期间到达的写入会被永久
// 跳过——对端按这个位点要增量，就再也拿不到它们了。
func TestCommitLeavesAnchorAtSeqBeforeCommit(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	writeDaily(t, srv, tok, "a.md", "A")

	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	seqBefore := l.Last()
	if seqBefore == 0 {
		t.Fatalf("写入之后位点应大于 0")
	}

	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := srv.commitBranch("daily", repo, "zero", "test commit")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if sha == "" {
		t.Fatalf("commit 应返回 SHA")
	}

	got, ok := l.SeqForCommit(sha)
	if !ok {
		t.Fatalf("提交 %s 之后应能查到锚点", shortSHA(sha))
	}
	if got != seqBefore {
		t.Fatalf("锚点位点应取提交之前的 %d，实际 %d", seqBefore, got)
	}

	// 短 SHA 也要能解析：人手上拿到的往往是短号。
	if _, ok := l.SeqForCommit(sha[:8]); !ok {
		t.Fatalf("短 SHA 前缀应能解析到锚点")
	}

	// 长度不够当不了前缀的输入必须被拒绝，不能瞎猜。
	if _, ok := l.SeqForCommit(""); ok {
		t.Fatalf("空字符串不应命中任何锚点")
	}
}

// 给了已知提交，导出的必须是「该提交之后」的增量，而不是整树。
func TestExportFromCommitSendsOnlyDelta(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	writeDaily(t, srv, tok, "a.md", "A")
	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	sha1, err := srv.commitBranch("daily", repo, "zero", "first")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 提交之后再写一笔：这笔不属于 sha1，必须能被增量导出。
	writeDaily(t, srv, tok, "b.md", "B")

	batch, err := srv.exportOpsFrom("daily", "zero", 0, sha1)
	if err != nil {
		t.Fatal(err)
	}
	if batch.BaseCommit != sha1 {
		t.Fatalf("响应应回显基准提交 %s，实际 %q", shortSHA(sha1), batch.BaseCommit)
	}
	if len(batch.Ops) == 0 {
		t.Fatalf("提交之后的写入应被导出，实际 0 条命令")
	}
	for _, op := range batch.Ops {
		if op.Kind == opTree {
			t.Fatalf("已知基准时不应退回整树，收到 opTree（Replace=%v）", op.Replace)
		}
	}
	// 位点必须都在锚点之后。
	l, lerr := srv.opLogs.For("daily", "zero")
	if lerr != nil {
		t.Fatal(lerr)
	}
	seq, ok := l.SeqForCommit(sha1)
	if !ok {
		t.Fatal("锚点应存在")
	}
	for _, op := range batch.Ops {
		if op.Seq <= seq {
			t.Fatalf("导出命令的位点 %d 不应早于基准位点 %d", op.Seq, seq)
		}
	}
}

// 不认识的提交必须退化为整树——报错会让老客户端永远同步不了。
func TestExportFromUnknownCommitFallsBackToFullTree(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	writeDaily(t, srv, tok, "a.md", "A")
	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.commitBranch("daily", repo, "zero", "first"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	unknown := strings.Repeat("f", 40)
	batch, err := srv.exportOpsFrom("daily", "zero", 0, unknown)
	if err != nil {
		t.Fatalf("不认识的基准不应报错: %v", err)
	}
	if batch.BaseCommit == unknown {
		t.Fatalf("认不出的基准不该被回显成已满足")
	}
	var hasTree bool
	for _, op := range batch.Ops {
		if op.Kind == opTree && op.Replace {
			hasTree = true
		}
	}
	if !hasTree {
		t.Fatalf("不认识的基准必须退化为权威整树（opTree + Replace），实际 %d 条命令", len(batch.Ops))
	}
}

// 锚点必须跨重启存活：它落在磁盘上，不是内存里的临时状态。
func TestAnchorSurvivesReload(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	writeDaily(t, srv, tok, "a.md", "A")
	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := srv.commitBranch("daily", repo, "zero", "first")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	want, ok := l.SeqForCommit(sha)
	if !ok {
		t.Fatal("锚点应存在")
	}

	// 用同一份磁盘文件重建日志对象，模拟进程重启。
	l2 := newOpLog(l.dir, l.file)
	if err := l2.Load(); err != nil {
		t.Fatalf("重新加载: %v", err)
	}
	got, ok := l2.SeqForCommit(sha)
	if !ok {
		t.Fatalf("重启后锚点丢失：%s 查不到位点", shortSHA(sha))
	}
	if got != want {
		t.Fatalf("重启后锚点位点应保持 %d，实际 %d", want, got)
	}
}

// 同一个提交重复登记只能留一条：与 commit 的幂等性对齐。
func TestRecordAnchorIsIdempotent(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	writeDaily(t, srv, tok, "a.md", "A")
	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := srv.commitBranch("daily", repo, "zero", "first")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	before := len(l.Anchors())
	if err := l.RecordAnchorAt(sha, 1); err != nil {
		t.Fatalf("重复登记不应报错: %v", err)
	}
	if after := len(l.Anchors()); after != before {
		t.Fatalf("重复登记同一个提交不该新增锚点：%d -> %d", before, after)
	}
}
