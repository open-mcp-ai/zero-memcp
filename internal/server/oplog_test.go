package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 命令流的端到端验证。
//
// 这些测试钉住的是「命令流 = 跨机器一致性的唯一内部抽象」：
//
//   - 单条写入 → 一个位点 → 对端按位点增量取走
//   - 位点单调且持久（重启后续编号）
//   - 重复投递幂等（同一位点重放不产生新变化）
//   - 删除与写入同一条流
//   - 整树同步 = 一条批量命令（历史层与增量层共用同一抽象）
//   - blob 缺失时不落地、验真后重放

func TestOpLogAssignsMonotonicSeqAndPersists(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	write := func(path, body string) {
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
	write("a.md", "A")
	write("b.md", "B")

	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	ops := l.All()
	if len(ops) < 2 {
		t.Fatalf("两次写入应各产生一条命令，实际 %d 条", len(ops))
	}
	for i := 1; i < len(ops); i++ {
		if ops[i].Seq != ops[i-1].Seq+1 {
			t.Fatalf("位点必须连续单调：%d 之后是 %d", ops[i-1].Seq, ops[i].Seq)
		}
	}
	last := l.Last()

	// 重启日志（同一份磁盘文件）：位点必须续上，不能从 1 重来。
	l2 := newOpLog(l.dir, l.file)
	if err := l2.Load(); err != nil {
		t.Fatalf("重新加载日志: %v", err)
	}
	if l2.Last() != last {
		t.Fatalf("重启后位点应续上 %d，实际 %d", last, l2.Last())
	}
	if l2.Len() != l.Len() {
		t.Fatalf("重启后命令条数应一致：%d vs %d", l.Len(), l2.Len())
	}
}

func TestOpLogIncrementalExport(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	for i := 0; i < 3; i++ {
		if _, err := callMCPRaw(t, srv, tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  fmt.Sprintf("write daily@zero:f%d.md $BODY", i),
				"vars": map[string]string{"BODY": fmt.Sprintf("内容 %d", i)},
			},
		}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	b0, err := srv.exportOps("daily", "zero", 0)
	if err != nil {
		t.Fatal(err)
	}
	total := len(b0.Ops)
	if total < 3 {
		t.Fatalf("应有至少 3 条命令，实际 %d", total)
	}

	// 增量：位点之后只能拿到剩下的，且不重发已给的。
	b1, err := srv.exportOps("daily", "zero", b0.Ops[0].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(b1.Ops) != total-1 {
		t.Fatalf("增量应少一条：%d vs %d", len(b1.Ops), total-1)
	}
	for _, op := range b1.Ops {
		if op.Seq <= b0.Ops[0].Seq {
			t.Fatalf("增量里出现了已同步过的位点 %d", op.Seq)
		}
	}

	// 已追平：空批次。
	b2, err := srv.exportOps("daily", "zero", b0.To)
	if err != nil {
		t.Fatal(err)
	}
	if len(b2.Ops) != 0 {
		t.Fatalf("追平后应为空批次，实际 %d 条", len(b2.Ops))
	}
}

// TestOpsSyncUncommittedAcrossNodes 是核心狗粮：
// A 写一条不 commit，B 通过命令流拉到，能读、能检索。
func TestOpsSyncUncommittedAcrossNodes(t *testing.T) {
	a, aDir := newWatchTestServer(t)
	b, bDir := newWatchTestServer(t)
	tokA := mintRestricted(t, aDir, auth.Claims{Sub: "zero", Name: "a", Cap: "rwx"})
	tokB := mintRestricted(t, bDir, auth.Claims{Sub: "zero", Name: "b", Cap: "rwx"})

	// A 写入，不提交。
	if _, err := callMCPRaw(t, a, tokA, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write daily@zero:note.md $BODY",
			"vars": map[string]string{"BODY": "未提交也要同步"},
		},
	}); err != nil {
		t.Fatalf("A write: %v", err)
	}

	tsA := httptest.NewServer(a.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer tsA.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	if _, err := b.pullOpsFromPeer(client, tsA.URL, tokA, "a", "daily", "zero"); err != nil {
		t.Fatalf("B pull: %v", err)
	}

	// B 必须读得到正文。
	out, err := callMCP(t, b, tokB, "read daily@zero:note.md")
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	if !strings.Contains(out, "未提交也要同步") {
		t.Fatalf("B 读到的内容不对:\n%s", out)
	}
	// 且仍然是未提交状态（命令流不制造 commit）。
	repoB, _ := b.projects.Get("daily")
	if repoB.Pending("zero") == 0 {
		t.Fatal("同步过来的内容应落在工作层，不应产生 commit")
	}

	// 再拉一次：没有新命令，不该重复应用。
	res2, err := b.pullOpsFromPeer(client, tsA.URL, tokA, "a", "daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Missing) != 0 || res2.Applied != 0 {
		t.Fatalf("追平后不应再有应用：applied=%d missing=%d", res2.Applied, len(res2.Missing))
	}
}

// TestOpsSyncDeleteIsACommand 锁定「删除不是特例」。
func TestOpsSyncDeleteIsACommand(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write daily@zero:del.md $BODY",
			"vars": map[string]string{"BODY": "待删"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "delete daily@zero:del.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	l, _ := srv.opLogs.For("daily", "zero")
	var sawSet, sawDel bool
	for _, op := range l.All() {
		if op.Path != "del.md" {
			continue
		}
		switch op.Kind {
		case opSet:
			sawSet = true
		case opDelete:
			sawDel = true
		}
	}
	if !sawSet || !sawDel {
		t.Fatalf("写入与删除都应在命令流里：set=%v delete=%v", sawSet, sawDel)
	}
}

// TestTreeSyncAsBatchOp 是你提出的关键抽象：
//
//	一棵对端完全没有的树，在命令流里就是一条批量命令。
//	历史层与增量层因此共用同一个底层。
func TestTreeSyncAsBatchOp(t *testing.T) {
	a, aDir := newWatchTestServer(t)
	tokA := mintRestricted(t, aDir, auth.Claims{Sub: "zero", Name: "a", Cap: "rwx"})

	for _, n := range []string{"x.md", "y.md", "sub/z.md"} {
		if _, err := callMCPRaw(t, a, tokA, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  fmt.Sprintf("write daily@zero:%s $BODY", n),
				"vars": map[string]string{"BODY": "正文 " + n},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}

	// 整树 → 一条批量命令。
	if err := a.noteTreeOp("daily", "zero"); err != nil {
		t.Fatalf("noteTreeOp: %v", err)
	}
	l, _ := a.opLogs.For("daily", "zero")
	ops := l.All()
	last := ops[len(ops)-1]
	if last.Kind != opTree {
		t.Fatalf("最后一条应是批量树命令，实际 %s", last.Kind)
	}
	if len(last.Batch) < 3 {
		t.Fatalf("批量命令应含整棵树（>=3 项），实际 %d", len(last.Batch))
	}

	// 应用到一台全新的空节点。
	b, bDir := newWatchTestServer(t)
	res, err := b.ingestOps("daily", "zero", opBatch{
		Project: "daily", Branch: "zero", Node: "a", Ops: []treeOp{last}, To: last.Seq,
	})
	if err != nil {
		t.Fatalf("ingest 批量命令: %v", err)
	}
	// 对象本地没有 → 全部进 Missing，但**绝不能**落地指向空气的条目。
	if len(res.Missing) < 3 {
		t.Fatalf("空节点应报告缺失对象，实际 %d", len(res.Missing))
	}
	repoB, _ := b.projects.Get("daily")
	if got := repoB.Pending("zero"); got != 0 {
		t.Fatalf("对象未到位时不应写入工作层，实际 pending=%d", got)
	}

	// 补齐对象后重放：整棵树一次出现。
	for _, e := range last.Batch {
		body, berr := mustBlob(t, a, e.SHA)
		if berr != nil {
			t.Fatal(berr)
		}
		if err := b.applyWorkBlob("daily", e.SHA, body); err != nil {
			t.Fatalf("补对象 %s: %v", e.SHA, err)
		}
	}
	res2, err := b.applyOps("daily", "zero", "a", []treeOp{last})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Missing) != 0 {
		t.Fatalf("对象补齐后不应再有缺失: %v", res2.Missing)
	}
	tokB := mintRestricted(t, bDir, auth.Claims{Sub: "zero", Name: "b", Cap: "rwx"})
	out, err := callMCP(t, b, tokB, "read daily@zero:sub/z.md")
	if err != nil {
		t.Fatalf("批量同步后读取: %v", err)
	}
	if !strings.Contains(out, "正文 sub/z.md") {
		t.Fatalf("内容不对:\n%s", out)
	}
}

// TestOpLogRejectsCorruptSeq 锁定「缺口不能静默」。
func TestOpLogRejectsCorruptSeq(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "b.ndjson")
	l := newOpLog(dir, file)
	if _, err := l.Append(treeOp{Node: "n", Kind: opSet, Path: "a", SHA: "x"}); err != nil {
		t.Fatal(err)
	}
	// 手工造一条位点回退的日志。
	bad := `{"seq":1,"node":"n","kind":"set","path":"a","sha":"x"}` + "\n" +
		`{"seq":1,"node":"n","kind":"set","path":"b","sha":"y"}` + "\n"
	if err := osWrite(file, bad); err != nil {
		t.Fatal(err)
	}
	l2 := newOpLog(dir, file)
	if err := l2.Load(); err == nil {
		t.Fatal("位点不推进的日志必须报错，不能静默跳过")
	}
}

func mustBlob(t *testing.T, s *Server, sha string) ([]byte, error) {
	t.Helper()
	repo, err := s.projects.Get("daily")
	if err != nil {
		return nil, err
	}
	return repo.BlobBody(sha)
}
