package gitstore

import (
	"path/filepath"
	"testing"
)

// TestMergeFastForward 验证快进：本地落后时直接把指针推到对端。
func TestMergeFastForward(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(filepath.Join(dir, "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// main: 提交 1
	if err := r.Write("main", "a.md", []byte("base"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c1, _ := r.Commit("main", "c1")

	// peer 基于 main 前进两步：先用 main 的 commit 当底座
	if err := r.updateRef("refs/heads/peer", c1); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("peer", "b.md", []byte("from peer"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c2, _ := r.Commit("peer", "c2")

	res, err := r.MergeInto("main", "peer")
	if err != nil {
		t.Fatalf("MergeInto: %v", err)
	}
	if res.Kind != "fast-forward" {
		t.Fatalf("应为 fast-forward，得到 %s", res.Kind)
	}
	if res.Commit != c2 {
		t.Fatalf("fast-forward 后 commit 应为 %s，得到 %s", c2, res.Commit)
	}
	// 验证内容可读
	b, err := r.Read("main", "b.md")
	if err != nil || string(b) != "from peer" {
		t.Fatalf("合并后内容异常: %s, err: %v", string(b), err)
	}
}

// TestMergeDisjointDiverged 验证两边改动正交（分叉但不冲突）时成功生成 merge commit。
// 这就是 main.record 的日常合并场景：两台机器各写各的记录，路径天然不同。
func TestMergeDisjointDiverged(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(filepath.Join(dir, "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// 共同祖先
	if err := r.Write("main", "seed.md", []byte("seed"), ModeContent); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Commit("main", "seed")

	// 基于 seed 建 peer 分支
	peerSHA, _ := r.Resolve("main")
	_ = r.updateRef("refs/heads/peer", peerSHA)

	// 本机写 laptop.md
	if err := r.Write("main", "record/laptop.md", []byte("from laptop"), ModeContent); err != nil {
		t.Fatal(err)
	}
	cLocal, _ := r.Commit("main", "from laptop")

	// 对端写 desktop.md
	if err := r.Write("peer", "record/desktop.md", []byte("from desktop"), ModeContent); err != nil {
		t.Fatal(err)
	}
	cPeer, _ := r.Commit("peer", "from desktop")

	res, err := r.MergeInto("main", "peer")
	if err != nil {
		t.Fatalf("MergeInto: %v", err)
	}
	if res.Kind != "merged" {
		t.Fatalf("应为 merged，得到 %s", res.Kind)
	}

	// 验证 merge commit 的两个 parent
	c, err := r.commitOf("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Parents) != 2 || c.Parents[0] != cLocal || c.Parents[1] != cPeer {
		t.Fatalf("merge commit 应有两个 parent [%s, %s]，得到 %v", cLocal, cPeer, c.Parents)
	}

	// 两边的文件都应该在
	for _, p := range []string{"seed.md", "record/laptop.md", "record/desktop.md"} {
		if _, err := r.Read("main", p); err != nil {
			t.Errorf("合并后缺少文件 %s: %v", p, err)
		}
	}
}

// TestMergeConflictDetected 验证同一文件被两边以不同内容改动时拒绝合并并报出路径。
// 这是 main 人格分支的保护防线：绝不悄悄覆盖。
func TestMergeConflictDetected(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(filepath.Join(dir, "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("main", "rules.md", []byte("v1"), ModeContent); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Commit("main", "seed")
	seedSHA, _ := r.Resolve("main")
	_ = r.updateRef("refs/heads/peer", seedSHA)

	// 本机改成 v2-laptop
	if err := r.Write("main", "rules.md", []byte("v2-laptop"), ModeContent); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Commit("main", "v2-laptop")

	// 对端改成 v2-desktop
	if err := r.Write("peer", "rules.md", []byte("v2-desktop"), ModeContent); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Commit("peer", "v2-desktop")

	res, err := r.MergeInto("main", "peer")
	if err != nil {
		t.Fatalf("不应返回 Go 错误，应该返回结构化 conflict: %v", err)
	}
	if res.Kind != "conflict" {
		t.Fatalf("应为 conflict，得到 %s", res.Kind)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "rules.md" {
		t.Fatalf("冲突路径应为 [rules.md]，得到 %v", res.Conflicts)
	}

	// 分支指针不应改变，本机内容应保留为 v2-laptop
	b, err := r.Read("main", "rules.md")
	if err != nil || string(b) != "v2-laptop" {
		t.Fatalf("冲突时本地分支不应被污染: %s, err: %v", string(b), err)
	}
}

// TestMergeRefusesDirty 本地脏层有未提交修改时拒绝合并。
func TestMergeRefusesDirty(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(filepath.Join(dir, "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("main", "a.md", []byte("clean"), ModeContent); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Commit("main", "c1")
	seedSHA, _ := r.Resolve("main")
	_ = r.updateRef("refs/heads/peer", seedSHA)

	// 本机写了脏层但不提交
	if err := r.Write("main", "dirty.md", []byte("uncommitted"), ModeContent); err != nil {
		t.Fatal(err)
	}

	_, err = r.MergeInto("main", "peer")
	if err == nil {
		t.Fatal("有脏层时应拒绝合并，却成功了")
	}
}
