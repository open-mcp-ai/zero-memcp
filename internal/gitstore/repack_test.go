package gitstore

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 打包后必须还能按 SHA 读出每一个对象——这是唯一重要的判据。
func TestRepackKeepsEveryObjectReadable(t *testing.T) {
	repo, err := Init(filepath.Join(t.TempDir(), "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	var heads []string
	for i := 0; i < 20; i++ {
		name := "note-" + string(rune('a'+i)) + ".md"
		if err := repo.Write("main", name, []byte("第 "+name+" 条记忆\n"), ModeContent); err != nil {
			t.Fatal(err)
		}
		sha, err := repo.Commit("main", "提交 "+name)
		if err != nil {
			t.Fatal(err)
		}
		heads = append(heads, sha)
	}

	res, err := repo.Repack()
	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}
	if res.Objects == 0 || res.Removed == 0 {
		t.Fatalf("结果异常: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(repo.Path, "objects", "pack", res.PackName+".idx")); err != nil {
		t.Fatalf("索引没写出来: %v", err)
	}

	for _, sha := range heads {
		if !repo.HasObject(sha) {
			t.Fatalf("打包后对象 %s 读不到", sha)
		}
		if typ, _, err := repo.readObject(sha); err != nil || typ != "commit" {
			t.Fatalf("读对象 %s 失败: %v (%s)", sha, err, typ)
		}
	}

	// 打包之后再写新对象也必须可读（松散与 pack 共存）。
	if err := repo.Write("main", "after.md", []byte("打包之后\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main", "打包后再提交"); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Resolve("main")
	if err != nil || head == "" {
		t.Fatalf("打包后提交不可读: %v", err)
	}
}

// 已有 pack 的仓库再打包：不能把上一个 pack 里的对象弄丢。
func TestRepackIsIdempotentAcrossPacks(t *testing.T) {
	repo, err := Init(filepath.Join(t.TempDir(), "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main", "a.md", []byte("甲\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	first, err := repo.Commit("main", "第一")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Repack(); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main", "b.md", []byte("乙\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	second, err := repo.Commit("main", "第二")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Repack(); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{first, second} {
		if !repo.HasObject(sha) {
			t.Fatalf("二次打包丢了 %s", sha)
		}
	}
}

// 我们写出的 pack/idx 必须能被真正的 git 读懂。
// 反过来说也成立：这个测试挂了，说明仓库对任何外部工具都坏了。
func TestRepackProducesValidGitPack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo, err := Init(filepath.Join(t.TempDir(), "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main", "a.md", []byte("内容\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main", "提交"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Repack(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "-C", repo.Path, "fsck", "--no-progress")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git fsck 失败: %v: %s", err, out)
	}
	cmd = exec.Command("git", "-C", repo.Path, "cat-file", "--batch-all-objects", "--batch-check")
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("git cat-file 失败: %v: %s", err, out)
	}
	if len(out) == 0 {
		t.Fatal("打包后 git 看不到任何对象")
	}
}
