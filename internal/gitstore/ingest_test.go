package gitstore

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 用一个真实的 git 生成的 pack 验证解析：这是「别人送来的一包对象」，
// 本仓库不调 git，但必须读得懂 git 写出来的东西。
func TestIngestPackFromRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	runGitTest(t, root, "init", work)
	writeFileTest(t, filepath.Join(work, "a.md"), "第一条记忆\n")
	runGitTest(t, work, "add", ".")
	runGitTest(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "first")
	writeFileTest(t, filepath.Join(work, "b.md"), "第二条记忆\n")
	runGitTest(t, work, "add", ".")
	runGitTest(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "second")

	packPath := filepath.Join(root, "out.pack")
	cmd := exec.Command("git", "-C", work, "pack-objects", "--all", "--stdout")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	if err := osWriteTest(packPath, out); err != nil {
		t.Fatal(err)
	}
	head := gitOutTest(t, work, "rev-parse", "HEAD")

	repo, err := Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := repo.IngestPack(out)
	if err != nil {
		t.Fatalf("接收 pack 失败: %v", err)
	}
	if res.Objects == 0 || res.Written == 0 {
		t.Fatalf("结果异常: %+v", res)
	}
	if !repo.HasObject(head) {
		t.Fatalf("HEAD %s 应已落盘", head)
	}
	typ, body, err := repo.readObject(head)
	if err != nil || typ != "commit" {
		t.Fatalf("读回 commit 失败: %v %s", err, typ)
	}
	if !bytes.Contains(body, []byte("second")) {
		t.Fatalf("commit 内容不对: %s", body)
	}

	// 幂等：同一包再来一次仍然成功。
	if _, err := repo.IngestPack(out); err != nil {
		t.Fatalf("重复接收应成功: %v", err)
	}
}

// 截断的 pack 必须被拒绝，且不能留下半个对象。
func TestIngestPackRejectsTruncated(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	runGitTest(t, root, "init", work)
	writeFileTest(t, filepath.Join(work, "a.md"), "内容\n")
	runGitTest(t, work, "add", ".")
	runGitTest(t, work, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "c")
	out, err := exec.Command("git", "-C", work, "pack-objects", "--all", "--stdout").Output()
	if err != nil {
		t.Fatal(err)
	}

	repo, err := Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.IngestPack(out[:len(out)-5]); err == nil {
		t.Fatal("截断的 pack 应被拒绝")
	}
	if _, err := repo.IngestPack([]byte("not a pack")); err == nil {
		t.Fatal("非 pack 数据应被拒绝")
	}
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func writeFileTest(t *testing.T, path, content string) {
	t.Helper()
	if err := osWriteTest(path, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

func osWriteTest(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func gitOutTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
