package server

import (
	"os"
	"path/filepath"
	"strings"
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

// 快照之后分支推进了：必须采用快照**并**把差量补上。
//
// 正确性底线是「新写的东西必须搜得到」，而不是「必须拒绝快照」。
//
// 演进过程：早先的判据是「所有分支 head 必须与快照一致，否则整份弃用」。
// 但这是活系统——后台每 30 秒折叠脏层、分身随时在写，11 条分支
// 一步没动过的概率接近零。实测两次启动都是
// 「main.dev moved since the snapshot; full rebuild」，快照一次没生效过。
//
// 现在改成应用差量：repo.Diff 是纯哈希比较（不读 blob），
// 改动的那几个 path 只重算那几个。
func TestIndexSnapshotAppliesDeltaWhenBranchMoved(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:a.md -- zzz-before-snapshot"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m one"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// 分支推进：新增一个文件、改掉另一个。
	if _, err := callMCP(t, srv, tok,
		"write blackegg/me@fact:b.md -- zzz-after-snapshot-marker"); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m two"); err != nil {
		t.Fatalf("commit two: %v", err)
	}

	if !srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("分支推进之后快照被整份弃用——活系统里这等于快照永不生效")
	}

	// 核心判据：快照之后新写的内容必须搜得到。
	out, err := callMCP(t, srv, tok, "recall zzz-after-snapshot-marker --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(out, "after-snapshot") && !contains(out, "zzz-after-snapshot-marker") {
		t.Fatalf("采用快照后新写的内容搜不到——这正是「启动很快但内容丢了」：%s", out)
	}

	// 快照之前就有的内容也必须在（差量不能把旧数据冲掉）。
	out, err = callMCP(t, srv, tok, "recall zzz-before-snapshot --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("recall before: %v", err)
	}
	if !contains(out, "before-snapshot") {
		t.Fatalf("应用差量把快照里的旧内容弄丢了：%s", out)
	}
}

// 删除也要跟着差量走：快照之后被删的文件不能再搜到。
func TestIndexSnapshotDeltaHandlesDeletion(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:gone.md -- zzz-will-be-deleted"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m one"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := callMCP(t, srv, tok, "delete blackegg/me@fact:gone.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m two"); err != nil {
		t.Fatalf("commit two: %v", err)
	}

	if !srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("删除之后快照不该整份弃用")
	}
	out, err := callMCP(t, srv, tok, "grep zzz-will-be-deleted --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if contains(out, "zzz-will-be-deleted") {
		t.Fatalf("差量没处理删除：快照之后删掉的内容还能搜到：%s", out)
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

	liveDocs, liveTerms := srv.idxNow().Docs(), srv.idxNow().Terms()

	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("快照应当被采用")
	}
	snapDocs, snapTerms := srv.idxNow().Docs(), srv.idxNow().Terms()

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

// 快照必须保住 Layer，否则新判据对历史数据完全失效且不报错。
//
// 这是 2026-11-16 的真实事故：改完 isSemanticBlock 之后部署，
// 语义维护日志仍然是 `delta: 16167 reusable, 0 need embed`——
// 候选集根本没变大。原因是 BlockSnapshot 里没有 Layer 字段，
// 从快照恢复的块 Layer 全是空串，判据依旧失效。
//
// 症状的形状很坏：**启动很快（命中快照）、日志全绿（0 error）、
// 功能完全不生效**。这正是我们今晚反复踩的那种「绿得不对」。
func TestSnapshotPreservesLayer(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 一条落在日期根目录、但自己声明了 layer: thought 的内容
	addr := "blackegg/me@fact:2026-11-16/declared.md"
	body := "---\nlayer: thought\nsession: s1\n---\n# Reasoning\n\nthis should be indexed\n"
	if _, err := callMCPVars(t, srv, tok, addr, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m one"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 存快照前：Layer 必须是 thought
	var wantLayer string
	func() {
		for _, tr := range testTrees(srv) {
			for _, b := range tr.Blocks {
				if strings.HasSuffix(b.Path, "declared.md") {
					wantLayer = b.Layer
				}
			}
		}
	}()
	if wantLayer != "thought" {
		t.Fatalf("扫描期没解析出 Layer：got %q，want thought", wantLayer)
	}

	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// 清空内存后从快照恢复
	srv.publishTrees(nil, nil)

	if !srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("快照没能命中——这条测试测不到恢复路径")
	}

	// 恢复之后 Layer 必须还在，否则 isSemanticBlock 对这条永远返回 false
	var gotLayer string
	var semantic bool
	func() {
		for _, tr := range testTrees(srv) {
			for _, b := range tr.Blocks {
				if strings.HasSuffix(b.Path, "declared.md") {
					gotLayer = b.Layer
					semantic = isSemanticBlock(b)
				}
			}
		}
	}()
	if gotLayer != "thought" {
		t.Fatalf("快照恢复丢了 Layer：got %q，want thought——"+
			"症状是「启动很快、日志全绿、新判据对历史数据完全不生效」", gotLayer)
	}
	if !semantic {
		t.Fatal("恢复后 isSemanticBlock 返回 false——日期根目录 + layer: thought 必须参与索引")
	}
}
