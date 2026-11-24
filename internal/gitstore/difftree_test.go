package gitstore

import "testing"

// 差分必须能直接吃两棵裸树 SHA。
//
// 命令流同步靠的就是这个：对端手上那棵树不是任何提交的 head，
// 它是内容寻址算出来的。只认 commit 的话这条路直接断掉，
// 只能退回重发整棵树——实测每次提交后 18,446 条里只有 1 条真的变了。
func TestDiffAcceptsTreeSHAs(t *testing.T) {
	r, err := Init(t.TempDir() + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Write("zero", "a.md", []byte("A"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "b.md", []byte("B"), ModeContent); err != nil {
		t.Fatal(err)
	}
	first, err := r.Commit("zero", "first")
	if err != nil {
		t.Fatal(err)
	}
	t1, err := r.TreeOfCommit(first)
	if err != nil || t1 == "" {
		t.Fatalf("取第一棵树: %s %v", t1, err)
	}

	// 改一个、删一个、加一个
	if err := r.Write("zero", "a.md", []byte("A2"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete("zero", "b.md"); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "c.md", []byte("C"), ModeContent); err != nil {
		t.Fatal(err)
	}
	second, err := r.Commit("zero", "second")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := r.TreeOfCommit(second)
	if err != nil || t2 == "" {
		t.Fatalf("取第二棵树: %s %v", t2, err)
	}
	if t1 == t2 {
		t.Fatal("前提不成立：两棵树不该相同")
	}

	changes, err := r.Diff(t1, t2)
	if err != nil {
		t.Fatalf("树到树差分失败（这是本次要修的核心能力）: %v", err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Path] = c.Kind
	}
	want := map[string]string{"a.md": "modified", "b.md": "deleted", "c.md": "added"}
	if len(got) != len(want) {
		t.Fatalf("差分条数不对：got=%v want=%v", got, want)
	}
	for p, k := range want {
		if got[p] != k {
			t.Fatalf("%s 的变化应是 %s，实际 %q（全套 %v）", p, k, got[p], got)
		}
	}
	// set 命令要靠 Hash/Mode 才能落地，缺了就等于没差分
	for _, c := range changes {
		if c.Kind == "deleted" {
			continue
		}
		if c.Hash == "" || c.Mode == "" {
			t.Fatalf("%s 的差分没带 Hash/Mode（%+v），无法变成 set 命令", c.Path, c)
		}
	}
}
