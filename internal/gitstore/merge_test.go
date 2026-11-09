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
	// 顺序不再是 [local, other]：父提交按 SHA 排序（见 merge.go 的说明），
	// 只要求两个 parent 都在。
	if len(c.Parents) != 2 {
		t.Fatalf("merge commit 应有两个 parent，得到 %v", c.Parents)
	}
	got := map[string]bool{c.Parents[0]: true, c.Parents[1]: true}
	if !got[cLocal] || !got[cPeer] {
		t.Fatalf("merge commit 的两个 parent 应是 %s 与 %s，得到 %v", cLocal, cPeer, c.Parents)
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

// TestMergeCommitIsDeterministicAcrossPeers 钉住 merge 提交的确定性。
//
// 两个 peer 对同一对分叉提交各做一次 merge，必须得到**同一个 SHA**。
// 之前父顺序是 [local, other]：
//
//	A 视角：local=a, other=b → parent a, b
//	B 视角：local=b, other=a → parent b, a
//
// 树、消息、时间全同，只差父顺序，于是两个 SHA 不同：谁也推不上谁
// （非快进被拒）；而因为两边树相同，「树一样不写空提交」的检查会让
// 两边都判 up-to-date、各自抱住自己的提交——分叉永久僵持。
//
// 这里在同一个仓库里交替扮演两侧，强制走完整 merge 路径（不是快进）。
func TestMergeCommitIsDeterministicAcrossPeers(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(filepath.Join(dir, "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("main", "a.md", []byte("base"), ModeContent); err != nil {
		t.Fatal(err)
	}
	base, _ := r.Commit("main", "base")

	// A 侧
	if err := r.updateRef("refs/heads/main", base); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "from-a.md", []byte("A 写的"), ModeContent); err != nil {
		t.Fatal(err)
	}
	aCommit, _ := r.Commit("main", "a")

	// B 侧
	if err := r.updateRef("refs/heads/peer", base); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("peer", "from-b.md", []byte("B 写的"), ModeContent); err != nil {
		t.Fatal(err)
	}
	bCommit, _ := r.Commit("peer", "b")

	// A 视角：本地 main=aCommit，merge peer=bCommit
	resA, err := r.MergeInto("main", "peer")
	if err != nil || resA.Kind != "merged" {
		t.Fatalf("A 视角 merge: %v %+v", err, resA)
	}

	// 回滚成 B 视角：本地 main=bCommit，merge peer=aCommit
	if err := r.updateRef("refs/heads/main", bCommit); err != nil {
		t.Fatal(err)
	}
	if err := r.updateRef("refs/heads/peer", aCommit); err != nil {
		t.Fatal(err)
	}
	resB, err := r.MergeInto("main", "peer")
	if err != nil || resB.Kind != "merged" {
		t.Fatalf("B 视角 merge: %v %+v", err, resB)
	}

	if resA.Commit != resB.Commit {
		t.Fatalf("两个 peer 对同一对提交 merge 出了不同 SHA:\n  A 视角=%s\n  B 视角=%s\n这就是双机同步推不上去的根因", resA.Commit, resB.Commit)
	}
}

// TestMergeCommitTimestampNotBeforeParents 验证 merge 提交不比父提交更旧。
func TestMergeCommitTimestampNotBeforeParents(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(filepath.Join(dir, "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("main", "a.md", []byte("base"), ModeContent); err != nil {
		t.Fatal(err)
	}
	base, _ := r.Commit("main", "base")
	if err := r.Write("main", "a2.md", []byte("local"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "a"); err != nil {
		t.Fatal(err)
	}
	if err := r.updateRef("refs/heads/peer", base); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("peer", "b.md", []byte("peer"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("peer", "b"); err != nil {
		t.Fatal(err)
	}
	res, err := r.MergeInto("main", "peer")
	if err != nil || res.Kind != "merged" {
		t.Fatalf("merge: %v %v", err, res)
	}

	typ, body, err := r.readObject(res.Commit)
	if err != nil || typ != "commit" {
		t.Fatalf("读 merge 提交: %v", err)
	}
	c := parseCommit(body)
	for _, p := range c.Parents {
		_, pb, err := r.readObject(p)
		if err != nil {
			t.Fatal(err)
		}
		if pc := parseCommit(pb); c.When.Before(pc.When) {
			t.Fatalf("merge 提交时间 %s 早于父提交 %s，拓扑与时间顺序矛盾", c.When, pc.When)
		}
	}
}
