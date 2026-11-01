package gitstore

import (
	"os"
	"path/filepath"
	"testing"
)

// newRepo 建一个空裸仓库。
func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r.git")
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 未变化的子树，diff 必须是空——一个条目都不展开。
func TestSubtreeDiffShortCircuitsOnSameSHA(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "team/a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "other/b.md", []byte("b"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c1"); err != nil {
		t.Fatal(err)
	}
	c1, _ := r.Resolve("main")

	// 只动 other/，team/ 的子树 SHA 必须不变
	if err := r.Write("main", "other/b.md", []byte("b2"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c2"); err != nil {
		t.Fatal(err)
	}
	c2, _ := r.Resolve("main")

	changed, err := r.SubtreeChanged(c1, c2, "team")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("team 子树没动，不应判定为变化")
	}
	changes, err := r.SubtreeDiff(c1, c2, "team")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("未变化子树应返回空 diff，得到 %+v", changes)
	}

	changed, err = r.SubtreeChanged(c1, c2, "other")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("other 子树动过，应判定为变化")
	}
	changes, err = r.SubtreeDiff(c1, c2, "other")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Kind != "modified" {
		t.Fatalf("应得到一条 modified，实际 %+v", changes)
	}
}

func TestSubtreeDiffAddedAndDeleted(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "team/a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c1"); err != nil {
		t.Fatal(err)
	}
	c1, _ := r.Resolve("main")

	if err := r.Write("main", "team/b.md", []byte("b"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete("main", "team/a.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c2"); err != nil {
		t.Fatal(err)
	}
	c2, _ := r.Resolve("main")

	changes, err := r.SubtreeDiff(c1, c2, "team")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, c := range changes {
		kinds[c.Path] = c.Kind
	}
	if kinds["team/b.md"] != "added" || kinds["team/a.md"] != "deleted" {
		t.Fatalf("增删未正确识别: %+v", changes)
	}
}

func TestTreeAtMissingPathIsEmpty(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c1"); err != nil {
		t.Fatal(err)
	}
	sha, err := r.TreeAt("main", "nope/nothing")
	if err != nil {
		t.Fatal(err)
	}
	if sha != "" {
		t.Fatalf("不存在的路径应返回空 SHA，得到 %s", sha)
	}
	root, err := r.TreeAt("main", "")
	if err != nil {
		t.Fatal(err)
	}
	if root == "" {
		t.Fatal("根树不应为空")
	}
}

func TestTouchedUnderReportsOnlyChangedPrefixes(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "team/a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "other/b.md", []byte("b"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c1"); err != nil {
		t.Fatal(err)
	}

	if err := r.Write("main", "team/a.md", []byte("a2"), ModeContent); err != nil {
		t.Fatal(err)
	}
	sha, err := r.Commit("main", "c2")
	if err != nil {
		t.Fatal(err)
	}

	touched, err := r.TouchedUnder(sha, []string{"team", "other", "absent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 1 || touched[0] != "team" {
		t.Fatalf("只应报告 team，实际 %v", touched)
	}
}
