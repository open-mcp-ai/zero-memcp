package server

import (
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/index"
)

// 快照头部记的文档数必须与写进文件的数据同源。
//
// 这是 2026-11-19 那次「重启 40 分钟全量冷建」的根因回归测试。
//
// 事故链条：
//  1. saveIndexSnapshot 先调 ix.Docs() 记账，隔着一大段拷贝树的循环
//     才调 ix.TakeSnapshot() 取数据；
//  2. 索引内部无锁，这两步之间只要有并发写入，取到的数据就比账本多；
//  3. 下次启动看到「文件 37,683 vs 头部 37,682」，判为损坏、整份弃用。
//
// 修法是把头部计数改成取自 snap 自己（len(snap.Addrs)/len(snap.Terms)）。
// 这条测试直接读盘上的头部与数据，验证两者相等——不依赖任何内部变量。
func TestSnapshotHeaderCountsMatchDataFile(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	for i := 0; i < 5; i++ {
		if _, err := callMCP(t, srv, tok,
			"write blackegg/me@fact:h"+itoa(i)+".md -- zzz-heal-marker "+itoa(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	dir := filepath.Join(dataDir, "system", "indexsnap")
	hdr, err := index.LoadSnapshotHeader(dir)
	if err != nil {
		t.Fatalf("load header: %v", err)
	}
	idx, err := index.ReadBinary(index.SnapshotPath(dir))
	if err != nil {
		t.Fatalf("read data: %v", err)
	}
	if hdr.Docs != idx.Docs() {
		t.Fatalf("头部记账 %d 与数据文件 %d 不一致——正是这个偏差导致整份快照作废、全量冷建",
			hdr.Docs, idx.Docs())
	}
	if hdr.Terms != idx.Terms() {
		t.Fatalf("词表数不一致：头部 %d vs 数据 %d", hdr.Terms, idx.Terms())
	}
}

// 头部计数与数据文件对不上时，必须治愈而不是全量重建。
//
// 做法：手改盘上的头部（把 Docs 减 1），模拟线上那份「差一篇」的快照，
// 然后走真实启动路径。断言：
//  1. tryLoadIndexSnapshot 返回 true（采用快照，没有退回冷建）
//  2. 原有内容仍然搜得到（自愈没有把索引搞坏）
func TestSnapshotCountDriftHealsInsteadOfRebuild(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const marker = "zzz-drift-survives"
	for i := 0; i < 3; i++ {
		if _, err := callMCP(t, srv, tok,
			"write blackegg/me@fact:d"+itoa(i)+".md -- "+marker+" "+itoa(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// 篡改头部：把 Docs 减 1，制造「差一篇」的漂移。
	dir := filepath.Join(dataDir, "system", "indexsnap")
	hdr, err := index.LoadSnapshotHeader(dir)
	if err != nil {
		t.Fatalf("load header: %v", err)
	}
	if hdr.Docs < 2 {
		t.Fatalf("前置条件不足：只有 %d 篇", hdr.Docs)
	}
	hdr.Docs--
	if err := writeHeaderForTest(dir, hdr); err != nil {
		t.Fatalf("rewrite header: %v", err)
	}

	if ok := srv.tryLoadIndexSnapshot("zero"); !ok {
		t.Fatal("计数漂移被当成损坏整份弃用——应该治愈，否则线上就是 40 分钟冷建")
	}

	out, err := callMCP(t, srv, tok, "recall "+marker+" --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(out, marker) {
		t.Fatalf("治愈后搜不到原有内容——索引被治坏了：%s", out)
	}
}

// writeHeaderForTest 直接重写头部文件（仅测试用）。
//
// 不走 index.SaveSnapshot，是因为要刻意写一份「头体不一致」的文件；
// 生产路径已经不可能产出这种状态，只能在这里手工构造。
func writeHeaderForTest(dir string, hdr *index.SnapshotHeader) error {
	return index.SaveSnapshot(dir, hdr, nil)
}

// 孤儿卡片必须留证（不能静默删掉）。
//
// 场景：快照索引里多出一条「树里没有」的条目（模拟文件异常消失）。
// 自愈删掉它后，必须返回那条地址作样本，供日志留证；不能只返回一个总数。
func TestHealReportsDroppedOrphanAddresses(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:keep.md -- zzz-keep"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatal(err)
	}

	idx := srv.idxNow()
	const ghost = "blackegg/me@fact:vanished-without-trace.md"
	idx.Add(ghost, "phantom content")

	removed, _, orphans := srv.healIndexAgainstTrees(idx, srv.treeList())
	if removed != 1 {
		t.Fatalf("期望剔除 1 条孤儿，实际 %d", removed)
	}
	found := false
	for _, o := range orphans {
		if o == ghost {
			found = true
		}
	}
	if !found {
		t.Fatalf("孤儿被静默清掉了，样本里没有它的地址：%v", orphans)
	}
}
