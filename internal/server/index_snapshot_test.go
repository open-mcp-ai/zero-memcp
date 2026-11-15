package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 索引快照必须能在「什么都没变」时被采用，且搜得到原来的内容。
func TestIndexSnapshotRoundTrip(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const marker = "zzz-snapshot-survives"
	for i := 0; i < 4; i++ {
		if _, err := callMCP(t, srv, tok,
			"write blackegg/me@fact:snap"+itoa(i)+".md -- "+marker+" "+itoa(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	before, err := callMCP(t, srv, tok, "recall "+marker+" --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(before, marker) {
		t.Fatalf("快照前就搜不到：%s", before)
	}

	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	// 快照文件必须真的落盘（不然下面的加载是在测空气）。
	for _, name := range []string{"index-head.gob", "index-data.bin"} {
		p := filepath.Join(dataDir, "system", "indexsnap", name)
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("快照文件 %s 没写出来：%v", name, serr)
		}
	}

	// 走快照路径重建索引，内容必须一模一样。
	ok := srv.tryLoadIndexSnapshot("zero")
	if !ok {
		t.Fatal("什么都没变却拒绝采用快照——那样这个优化永远不会生效")
	}

	after, err := callMCP(t, srv, tok, "recall "+marker+" --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("recall after snapshot: %v", err)
	}
	if !contains(after, marker) {
		t.Fatalf("从快照恢复后搜不到——索引看起来建好了，实际是空的：%s", after)
	}
}

// 快照必须能被证伪：仓库推进之后，旧快照**不许**被采用。
//
// 这是这个优化的正确性底线。采用了过期快照的症状是
// 「启动很快，但刚写的东西搜不到」，而且完全不报错。
func TestIndexSnapshotRejectedWhenBranchMoved(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:a.md -- first"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m one"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// 分支推进。
	if _, err := callMCP(t, srv, tok,
		"write blackegg/me@fact:b.md -- zzz-after-snapshot-marker"); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m two"); err != nil {
		t.Fatalf("commit two: %v", err)
	}

	if srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("分支已经推进，旧快照仍然被采用——" +
			"症状会是「启动很快，但刚写的东西搜不到」，而且不报错")
	}
}

// 快照文件被破坏时必须退回全量重建，不能崩、不能采用半个文件。
func TestIndexSnapshotRejectedWhenCorrupt(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:c.md -- x"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m one"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// 把数据文件写坏（模拟「进程在写快照时被杀掉」）。
	p := filepath.Join(dataDir, "system", "indexsnap", "index-data.bin")
	if err := os.WriteFile(p, []byte("not a gob at all"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("数据文件已损坏却仍被采用")
	}

	// 头部也写坏。
	hp := filepath.Join(dataDir, "system", "indexsnap", "index-head.gob")
	if err := os.WriteFile(hp, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("corrupt header: %v", err)
	}
	if srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("头部已损坏却仍被采用")
	}
}

// 快照不能成为「权限」的来源：SKILL.md 的策略必须从磁盘重新解析。
func TestIndexSnapshotDoesNotCarryPolicy(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCPVars(t, srv, tok, "blackegg/me@fact:SKILL.md",
		"---\ninject: every_turn\n---\n\npolicy v1"); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m one"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 清掉内存里的 policy，模拟「新进程刚起来」。
	srv.policies.Delete("blackegg/me")
	if _, ok := srv.policies.Load("blackegg/me"); ok {
		t.Fatal("policy 没清掉，测试前提不成立")
	}

	if !srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("快照应当被采用")
	}
	if _, ok := srv.policies.Load("blackegg/me"); !ok {
		t.Fatal("从快照恢复之后没有重新解析 SKILL.md——" +
			"权限绝不能来自一份可能过期的快照")
	}
}

// 快照读回来的东西必须与内存里的完全等价：doc 数、词数、检索结果。
func TestIndexSnapshotMatchesLiveIndex(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	for i := 0; i < 6; i++ {
		if _, err := callMCP(t, srv, tok,
			"write blackegg/me@fact:eq"+itoa(i)+".md -- zzz-eq-marker unique"+itoa(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	srv.mu.RLock()
	liveDocs, liveTerms := srv.idx.Docs(), srv.idx.Terms()
	srv.mu.RUnlock()

	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("快照应当被采用")
	}
	srv.mu.RLock()
	snapDocs, snapTerms := srv.idx.Docs(), srv.idx.Terms()
	srv.mu.RUnlock()

	if liveDocs != snapDocs || liveTerms != snapTerms {
		t.Fatalf("快照与内存索引不等价：live docs=%d terms=%d，snapshot docs=%d terms=%d",
			liveDocs, liveTerms, snapDocs, snapTerms)
	}
}

// 快照保存不能阻塞太久：它挂在后台维护循环上。
func TestIndexSnapshotSaveIsFast(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})
	for i := 0; i < 50; i++ {
		if _, err := callMCP(t, srv, tok,
			"write blackegg/me@fact:sp"+itoa(i)+".md -- body "+itoa(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t0 := time.Now()
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if el := time.Since(t0); el > 5*time.Second {
		t.Fatalf("存快照耗时 %v，太慢（它跑在后台维护循环上）", el)
	}
}
