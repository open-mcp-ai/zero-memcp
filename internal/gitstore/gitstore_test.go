package gitstore

import (
	"os"
	"path/filepath"
	"testing"
)

func newRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(filepath.Join(t.TempDir(), "mem.git"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestInitIsBare(t *testing.T) {
	r := newRepo(t)
	for _, want := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(r.Path, want)); err != nil {
			t.Fatalf("裸仓库缺 %s: %v", want, err)
		}
	}
	// 裸仓库没有工作区
	if _, err := os.Stat(filepath.Join(r.Path, "..", "x")); err == nil {
		t.Fatal("不该有工作区")
	}
}

// 写入 → 提交 → 读回，这是整个存储层的地基。
func TestWriteCommitRead(t *testing.T) {
	r := newRepo(t)

	if err := r.Write("zero", "2026-10-29/a.md", []byte("第一条\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	// 提交前就该能读到——否则 agent 会以为写失败了，重复写。
	if got, err := r.Read("zero", "2026-10-29/a.md"); err != nil || string(got) != "第一条\n" {
		t.Fatalf("提交前应可读: %q %v", got, err)
	}

	commit, err := r.Commit("zero", "mem: 第一条")
	if err != nil {
		t.Fatal(err)
	}
	if commit == "" {
		t.Fatal("应产生一个提交")
	}

	got, err := r.Read("zero", "2026-10-29/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "第一条\n" {
		t.Fatalf("内容不对: %q", got)
	}
}

// 第二次提交必须复用未改动的条目，只新增自己的对象。
func TestSecondCommitReusesTree(t *testing.T) {
	r := newRepo(t)

	if err := r.Write("zero", "a.md", []byte("a\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c1, err := r.Commit("zero", "c1")
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Write("zero", "b.md", []byte("b\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c2, err := r.Commit("zero", "c2")
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c2 {
		t.Fatal("第二次提交应产生新的 commit")
	}

	entries, err := r.List("zero")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("应有 2 个文件，得到 %d", len(entries))
	}

	// a.md 的 blob 哈希在两个提交里必须一样——没被重写。
	var aHash string
	for _, e := range entries {
		if e.Path == "a.md" {
			aHash = e.Hash
		}
	}
	if aHash == "" {
		t.Fatal("a.md 不在列表里")
	}
}

// 空提交不产生 commit：空提交会污染 log，让「上次提交时间」失去意义。
func TestCommitWithNothingPending(t *testing.T) {
	r := newRepo(t)
	c, err := r.Commit("zero", "empty")
	if err != nil {
		t.Fatal(err)
	}
	if c != "" {
		t.Fatalf("无待提交内容时应返回空串，得到 %q", c)
	}
}

// mode 要能往返：100755 表示「这条是规则」。
func TestModeRoundTrip(t *testing.T) {
	r := newRepo(t)
	if err := r.Write("zero", "rules/01.md", []byte("规则\n"), ModeRule); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "notes/01.md", []byte("笔记\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "两种 mode"); err != nil {
		t.Fatal(err)
	}

	entries, err := r.List("zero")
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]Mode{}
	for _, e := range entries {
		modes[e.Path] = e.Mode
	}
	if modes["rules/01.md"] != ModeRule {
		t.Fatalf("rules 应是 %s，得到 %s", ModeRule, modes["rules/01.md"])
	}
	if modes["notes/01.md"] != ModeContent {
		t.Fatalf("notes 应是 %s，得到 %s", ModeContent, modes["notes/01.md"])
	}
}

// 多分支互不干扰：人格就是分支。
func TestBranchesAreIndependent(t *testing.T) {
	r := newRepo(t)

	if err := r.Write("zero", "me.md", []byte("zero\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "zero 的"); err != nil {
		t.Fatal(err)
	}

	if err := r.Write("diana", "me.md", []byte("diana\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("diana", "diana 的"); err != nil {
		t.Fatal(err)
	}

	a, err := r.Read("zero", "me.md")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Read("diana", "me.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != "zero\n" || string(b) != "diana\n" {
		t.Fatalf("分支串了: zero=%q diana=%q", a, b)
	}

	refs, err := r.Refs("")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("应有 2 条 ref，得到 %v", refs)
	}
}

// 分叉人格是零字节复制：新分支指向同一个 commit。
func TestForkCostsNothing(t *testing.T) {
	r := newRepo(t)
	if err := r.Write("zero", "x.md", []byte("x\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c, err := r.Commit("zero", "base")
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Fork("zero", "diana"); err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve("diana")
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("分叉后应指向同一个 commit: 要 %s 得到 %s", c, got)
	}

	// 分叉后各自写，互不影响
	if err := r.Write("diana", "y.md", []byte("y\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("diana", "diana 的"); err != nil {
		t.Fatal(err)
	}
	zeroEntries, _ := r.List("zero")
	if len(zeroEntries) != 1 {
		t.Fatalf("zero 不该看到 diana 的写入，得到 %v", zeroEntries)
	}
}

// 路径校验：这两个字符会让 git 参数被重新解释。
func TestPathValidation(t *testing.T) {
	r := newRepo(t)
	bad := []string{
		"",
		"a:b",    // ref:path 分隔符
		"/abs",   // 绝对路径
		"../up",  // 上溯
		"a//b",   // 空段
		"a/./b",  // 点段
		"a\x00b", // 空字节
	}
	for _, p := range bad {
		if err := r.Write("zero", p, []byte("x"), ModeContent); err == nil {
			t.Fatalf("%q 应被拒绝", p)
		}
	}
}

// 不存在的 ref 列表应为空而不是报错。
func TestListMissingRef(t *testing.T) {
	r := newRepo(t)
	entries, err := r.List("nobody")
	if err != nil {
		t.Fatalf("不存在的 ref 不该报错: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("应为空，得到 %v", entries)
	}
}

// 一次读多个文件的结果必须与逐个读一致。
func TestReadMany(t *testing.T) {
	r := newRepo(t)
	want := map[string]string{
		"a.md":     "aaa\n",
		"b/c.md":   "ccc\n",
		"d/e/f.md": "fff\n",
	}
	for p, v := range want {
		if err := r.Write("zero", p, []byte(v), ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Commit("zero", "batch"); err != nil {
		t.Fatal(err)
	}

	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	got, err := r.ReadMany("zero", paths)
	if err != nil {
		t.Fatal(err)
	}
	for p, v := range want {
		if string(got[p]) != v {
			t.Fatalf("%s: 要 %q 得到 %q", p, v, got[p])
		}
	}
}

// 提交历史要能读出来，且时间递减。
func TestLog(t *testing.T) {
	r := newRepo(t)
	for i := 0; i < 3; i++ {
		if err := r.Write("zero", "x.md", []byte{byte('0' + i)}, ModeContent); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit("zero", "c"); err != nil {
			t.Fatal(err)
		}
	}
	log, err := r.Log("zero", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 3 {
		t.Fatalf("应有 3 条提交，得到 %d", len(log))
	}
}
