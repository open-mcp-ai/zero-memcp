package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 命令流的基准模型：
//
//	命令流 = { base_tree, ops[seq 1..N] }
//
// 提交把 ops 全部折进树，流随即归零到新基准、seq 从 1 重来。于是
// 「分支还没有任何提交」不是特例——基准就是 Git 的官方空树对象。
//
// 这批测试钉住的正是这几件事：
//
//   - 没有提交时基准是空树，不是「未知」
//   - 提交后流归零，基准变成新提交的树
//   - 已提交的内容不再躺在流里（否则日志无限增长）
//   - 两边基准不同时发整树快照，而不是发对不上的增量
//   - 基准相同的两端走增量

func writeDaily(t *testing.T, srv *Server, tok, path, body string) {
	t.Helper()
	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  fmt.Sprintf("write daily@zero:%s $BODY", path),
			"vars": map[string]string{"BODY": body},
		},
	}); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// 分支还没有任何提交时，基准是空树——一个确定的值，不是「未知」。
func TestBaseTreeIsEmptyTreeWhenNoCommit(t *testing.T) {
	srv, _ := newWatchTestServer(t)
	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.BaseTree(); got != gitstore.EmptyTreeSHA {
		t.Fatalf("没有提交时基准必须是空树 %s，实际 %s", shortSHA(gitstore.EmptyTreeSHA), shortSHA(got))
	}
	if l.Last() != 0 {
		t.Fatalf("没有写入时位点应为 0，实际 %d", l.Last())
	}
}

// 提交把命令折进树，流归零：seq 回到 0，基准变成新提交的树。
//
// 这是「命令流不会无限增长」的那个不变量。不归零的话，每次提交只是
// 在末尾追加，日志只增不减，而它记的内容早已经变成历史。
func TestCommitResetsStreamToNewBase(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	writeDaily(t, srv, tok, "a.md", "A")
	writeDaily(t, srv, tok, "b.md", "B")

	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	before := l.Last()
	if before < 2 {
		t.Fatalf("两次写入应产生至少 2 条命令，实际 %d", before)
	}
	if l.BaseTree() != gitstore.EmptyTreeSHA {
		t.Fatalf("提交前基准应仍是空树，实际 %s", shortSHA(l.BaseTree()))
	}

	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := srv.commitBranch("daily", repo, "zero", "first commit")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	wantTree, err := repo.TreeOfCommit(sha)
	if err != nil {
		t.Fatal(err)
	}

	if got := l.Last(); got != 0 {
		t.Fatalf("提交后流必须归零（seq 回到 0），实际 %d——未归零则日志会无限增长", got)
	}
	if got := l.BaseTree(); got != wantTree {
		t.Fatalf("提交后基准应变成新提交的树 %s，实际 %s", shortSHA(wantTree), shortSHA(got))
	}
	if n := len(l.All()); n != 0 {
		t.Fatalf("提交后流里不该残留命令（内容已在树上），实际 %d 条", n)
	}

	// 归零必须落盘：重启后仍是这个基准，不能又回到空树。
	l2 := newOpLog(l.dir, l.file)
	if err := l2.Load(); err != nil {
		t.Fatalf("重新加载: %v", err)
	}
	if got := l2.BaseTree(); got != wantTree {
		t.Fatalf("重启后基准应保持 %s，实际 %s", shortSHA(wantTree), shortSHA(got))
	}
	if got := l2.Last(); got != 0 {
		t.Fatalf("重启后位点应为 0，实际 %d", got)
	}
}

// 提交之后再写，新命令从 seq 1 开始——它们叠在新基准上。
func TestWriteAfterCommitStartsAtSeqOne(t *testing.T) {
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

	writeDaily(t, srv, tok, "b.md", "B")

	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	ops := l.All()
	if len(ops) != 1 {
		t.Fatalf("提交后只写了一笔，流里应只有 1 条命令，实际 %d", len(ops))
	}
	if ops[0].Seq != 1 {
		t.Fatalf("归零之后第一条命令的位点必须是 1，实际 %d", ops[0].Seq)
	}
	if ops[0].Path != "b.md" {
		t.Fatalf("流里不该还有 a.md（它已经在树上了），实际第一条是 %s", ops[0].Path)
	}
}

// 树没变就不归零：此时流里的命令还没进树，归零会把它们丢掉。
//
// 直接测守卫本身，不走 commit 路径——真实的空提交在 gitstore 那层就
// 返回空 SHA 了，走不到这里。
func TestResetSkippedWhenTreeUnchanged(t *testing.T) {
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

	writeDaily(t, srv, tok, "b.md", "B")
	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	baseBefore := l.BaseTree()
	if l.Last() != 1 {
		t.Fatalf("前提：应有一笔待提交命令，实际 %d", l.Last())
	}

	// 拿同一个提交再归零一次：树没变，必须原地不动。
	srv.resetOpLogToCommit("daily", "zero", repo, sha)

	if got := l.Last(); got != 1 {
		t.Fatalf("树没变时不该归零，流里的 1 条命令必须保留，实际 %d", got)
	}
	if got := l.BaseTree(); got != baseBefore {
		t.Fatalf("基准不该变，期望 %s 实际 %s", shortSHA(baseBefore), shortSHA(got))
	}
}

// 两边基准不同：发整树快照（Replace），不发对不上的增量。
//
// 两个节点各自提交过，基准必然不同——这不是异常，是常态。
func TestExportSendsSnapshotWhenBaseDiffers(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	writeDaily(t, srv, tok, "a.md", "A")
	writeDaily(t, srv, tok, "b.md", "B")
	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.commitBranch("daily", repo, "zero", "first"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	writeDaily(t, srv, tok, "c.md", "C")

	srv.opLogs.For("daily", "zero")

	// 用一个不可能相等的基准来拉
	batch, err := srv.exportOpsFrom("daily", "zero", 0, "1111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("基准不同不该报错: %v", err)
	}
	if !isSnapshotBatch(batch) {
		t.Fatalf("基准不同时必须发权威整树快照，实际 %d 条命令", len(batch.Ops))
	}
	paths := map[string]bool{}
	for _, e := range batch.Ops[0].Batch {
		paths[e.Path] = true
	}
	for _, want := range []string{"a.md", "b.md", "c.md"} {
		if !paths[want] {
			t.Fatalf("整树快照必须包含 %s，实际只有 %v", want, paths)
		}
	}
	if batch.BaseTree == "" {
		t.Fatalf("快照必须告诉我们它对应的基准树")
	}
}

// 基准相同：发增量，不发整树。
func TestExportSendsDeltaWhenBaseMatches(t *testing.T) {
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
	writeDaily(t, srv, tok, "b.md", "B")

	l, err := srv.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := srv.exportOpsFrom("daily", "zero", 0, l.BaseTree())
	if err != nil {
		t.Fatal(err)
	}
	if isSnapshotBatch(batch) {
		t.Fatalf("基准相同时应发增量，不该发整树快照")
	}
	if len(batch.Ops) != 1 || batch.Ops[0].Path != "b.md" {
		t.Fatalf("增量里应只有提交之后的 b.md，实际 %v", batch.Ops)
	}
	if batch.BaseTree != l.BaseTree() {
		t.Fatalf("响应应回显基准树")
	}
}

// 端到端：基准不同走快照，落地后对端基准对齐，再来就是增量。
func TestTwoNodesRebaseThenDelta(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	writeDaily(t, up, upTok, "a.md", "A")
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "up commit"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	writeDaily(t, up, upTok, "b.md", "B")

	ts := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	upLog, err := up.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}

	// 第一趟：下游用空树当基准去拉 → 上游基准已经变成提交的树 → 快照
	batch, err := postOpPullFrom(client, ts.URL, upTok, "daily", "zero", "down", 0, gitstore.EmptyTreeSHA, "")
	if err != nil {
		t.Fatalf("拉取: %v", err)
	}
	if !isSnapshotBatch(batch) {
		t.Fatalf("基准不同应返回整树快照，实际 %d 条命令", len(batch.Ops))
	}
	if batch.BaseTree != upLog.BaseTree() {
		t.Fatalf("快照应带上上游当前基准 %s，实际 %s", shortSHA(upLog.BaseTree()), shortSHA(batch.BaseTree))
	}

	// 第二趟：下游已经对齐基准 → 上游发增量
	batch2, err := postOpPullFrom(client, ts.URL, upTok, "daily", "zero", "down", 0, batch.BaseTree, "")
	if err != nil {
		t.Fatalf("第二次拉取: %v", err)
	}
	if isSnapshotBatch(batch2) {
		t.Fatalf("基准已对齐，不该再发整树快照")
	}
	var hasB bool
	for _, op := range batch2.Ops {
		if op.Path == "b.md" {
			hasB = true
		}
		if op.Path == "a.md" {
			t.Fatalf("a.md 已经在基准树里，不该出现在增量中")
		}
	}
	if !hasB {
		t.Fatalf("增量里应有提交之后的 b.md")
	}
}
