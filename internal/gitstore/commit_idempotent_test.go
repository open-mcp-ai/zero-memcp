package gitstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// failCleanupFS 让「清脏层」失败，模拟 ④ 之后 ⑤ 之前崩溃的形态。
type failCleanupFS struct {
	FS
	fail bool
}

func (f *failCleanupFS) RemoveAll(path string) error {
	if f.fail && strings.Contains(filepath.ToSlash(path), "/dirty/") {
		return errors.New("注入：清理脏层失败")
	}
	return f.FS.RemoveAll(path)
}

// Commit 必须在「ref 已前进但脏层没清掉」的形态下幂等。
//
// 这是注释一直声称、代码却没实现的性质：折叠是幂等的，所以重试
// 应当识别出「脏层描述的内容已经在 ref 里」，直接清掉它。
// 没有这个判定时，每次重试都多一个内容完全相同的提交，
// 而脏层永远清不掉——Pending 永远不为零，后台每轮都提交一次。
func TestCommitIsIdempotentAfterCleanupFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	fs := &failCleanupFS{FS: DefaultFS(), fail: true}
	r, err := InitWith(dir, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("zero", "a.md", []byte("内容"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}

	first, err := r.Commit("zero", "第一次")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("第一次提交应当产生一个 commit")
	}
	if got := r.Pending("zero"); got == 0 {
		t.Fatal("注入清理失败后脏层应当还在（这是本测试的前提）")
	}

	// 重试：内容与当前树一致，应当认出来，不再造新提交。
	second, err := r.Commit("zero", "重试")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("重试应当返回已有提交 %s，得到 %s（多造了一个空提交）", first, second)
	}

	log, err := r.Log("zero", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 {
		t.Errorf("应当只有 1 条提交，得到 %d 条", len(log))
	}

	// 清理仍然失败，所以脏层还在——这是底层错误，不是提交语义问题。
	// 但只要清理一恢复，重试就该把脏层收掉且不再新增提交。
	fs.fail = false
	third, err := r.Commit("zero", "再重试")
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Errorf("清理恢复后应当仍返回已有提交 %s，得到 %s", first, third)
	}
	if got := r.Pending("zero"); got != 0 {
		t.Errorf("清理恢复后脏层应当被收掉，还剩 %d 条", got)
	}
}

// 有真实改动时，幂等判定不能把改动吞掉。
func TestCommitStillCommitsRealChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	fs := &failCleanupFS{FS: DefaultFS(), fail: true}
	r, err := InitWith(dir, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("zero", "a.md", []byte("第一版"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	first, err := r.Commit("zero", "第一次")
	if err != nil {
		t.Fatal(err)
	}

	// 改内容（脏层里已有旧的 set，这次会覆盖它）
	if err := r.Write("zero", "a.md", []byte("第二版"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	second, err := r.Commit("zero", "第二次")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("内容变了却返回旧提交：幂等判定把真实改动吞掉了")
	}
	got, err := r.Read("zero", "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "第二版" {
		t.Errorf("读到 %q，要 %q", got, "第二版")
	}
}

// 分支名带路径穿越时，脏层目录也不能跑出仓库。
func TestCommitRejectsTraversalBranch(t *testing.T) {
	base := t.TempDir()
	repoPath := filepath.Join(base, "repos", "me.git")
	r, err := Init(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.Commit("../../evil", "x"); err == nil {
		t.Error("Commit 到带 .. 的分支应当被拒绝")
	}
}
