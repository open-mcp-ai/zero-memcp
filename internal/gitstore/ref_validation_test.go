package gitstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 分支名会直接变成 refs/heads/<name> 的文件路径。
//
// 这一组测试锁的是一个真实的越界写入：validRef 曾经定义了却没人调用
// （死代码），于是 Fork/Commit/SetRef 可以把 "../../../.." 当分支名，
// 把 ref 文件写到仓库外面去。名字变路径的地方必须校验，这是那类
// 「本地看着一切正常、装到服务器上才出问题」的典型。
func TestRefNameRejectsTraversal(t *testing.T) {
	bad := []string{
		"..",
		"../escape",
		"../../../../escape",
		"a/../../b",
		"....//....//x",
		".hidden",
		"trailing.",
		"a//b",
		"/leading",
		"trailing/",
		"has space",
		"has~tilde",
		"has^caret",
		"has:colon",
		"has?question",
		"has*star",
		"has[bracket",
		"has\\backslash",
		"has\ttab",
		"has\nnewline",
		"",
	}
	for _, name := range bad {
		if err := ValidateBranch(name); err == nil {
			t.Errorf("ValidateBranch(%q) 应当拒绝", name)
		}
	}

	good := []string{"zero", "main", "feat/x", "blackegg/me", "release-1.0", "a_b/c-d"}
	for _, name := range good {
		if err := ValidateBranch(name); err != nil {
			t.Errorf("ValidateBranch(%q) 不该拒绝: %v", name, err)
		}
	}
}

// Fork 的目标分支如果带路径穿越，绝不能写出仓库目录之外。
func TestForkCannotEscapeRepoDir(t *testing.T) {
	base := t.TempDir()
	repoPath := filepath.Join(base, "data", "repos", "me.git")
	r, err := Init(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("zero", "seed.md", []byte("seed"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}

	escapeTarget := filepath.Join(base, "data", "repos", "ESCAPED")
	for _, ref := range []string{
		"../../../../ESCAPED",
		"../../../ESCAPED",
		"....//....//ESCAPED",
	} {
		if err := r.Fork("zero", ref); err == nil {
			t.Errorf("Fork 到 %q 应当被拒绝", ref)
		}
		if _, statErr := os.Stat(escapeTarget); statErr == nil {
			t.Fatalf("ref 文件被写到仓库之外: %s", escapeTarget)
		}
	}
}

// Write/Delete 的分支名同样要校验：脏层目录也是按分支名拼路径的。
func TestWriteRejectsBadBranch(t *testing.T) {
	r, err := Init(filepath.Join(t.TempDir(), "repo.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("../../evil", "x.md", []byte("x"), ModeContent); err == nil {
		t.Error("Write 到带 .. 的分支应当被拒绝")
	}
	if err := r.Delete("../../evil", "x.md"); err == nil {
		t.Error("Delete 到带 .. 的分支应当被拒绝")
	}
	if _, err := r.Commit("../../evil", "x"); err == nil {
		t.Error("Commit 到带 .. 的分支应当被拒绝")
	}
}

// 脏层目录名同样来自分支名，越界会写出 data 目录。
func TestDirtyPathStaysInsideRepo(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repos", "me.git")
	r, err := Init(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// 即使绕过 Write 的校验直接算路径，也必须落在仓库目录内。
	for _, ref := range []string{"zero", "feat/x", "a/b/c"} {
		p := r.dirtyDirPath(ref)
		if !strings.HasPrefix(p, repoPath+string(os.PathSeparator)) {
			t.Errorf("脏层路径 %q 跑出了仓库 %q", p, repoPath)
		}
	}
}

// SetRef 是 git push 的落地入口，只该写仓库自己的命名空间。
func TestSetRefRejectsForeignNamespace(t *testing.T) {
	r, err := Init(filepath.Join(t.TempDir(), "repo.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	sha, err := r.writeObject("blob", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{
		"refs/config/sneaky",
		"refs/hooks/pre-commit",
		"../../outside",
		"refs/heads/../../outside",
	} {
		if err := r.SetRef(ref, sha); err == nil {
			t.Errorf("SetRef(%q) 应当被拒绝", ref)
		}
	}
	for _, ref := range []string{"refs/heads/zero", "refs/tags/v1", "refs/remotes/origin/zero"} {
		if err := r.SetRef(ref, sha); err != nil {
			t.Errorf("SetRef(%q) 不该被拒绝: %v", ref, err)
		}
	}
}
