package gittransport

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 两个推送方同时写同一个本地远端：ref 不能出现「读到旧值再覆盖」的丢失。
func TestLocalPushIsSerialisedByLock(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "backup.git")
	if _, err := gitstore.Init(remoteDir); err != nil {
		t.Fatal(err)
	}

	mk := func(name string) (*gitstore.Repo, string) {
		repo, err := gitstore.Init(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Write("zero", "a.md", []byte(name+"\n"), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
		sha, err := repo.Commit("zero", name)
		if err != nil {
			t.Fatal(err)
		}
		return repo, sha
	}
	repoA, _ := mk("a")
	repoB, _ := mk("b")

	// 先把 A 推上去，再让 B 推：B 不是快进，必须被拒绝而不是覆盖。
	if _, err := PushLocal(repoA, remoteDir, "zero"); err != nil {
		t.Fatalf("A 推送失败: %v", err)
	}
	if _, err := PushLocal(repoB, remoteDir, "zero"); err == nil {
		t.Fatal("B 的推送不是快进，应被拒绝")
	}
}

// 崩溃残留的锁文件不能永久卡住后续推送。
func TestStaleLockIsTakenOver(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "backup.git")
	if _, err := gitstore.Init(remoteDir); err != nil {
		t.Fatal(err)
	}
	// 一个几乎肯定不存在的 pid。
	if err := os.WriteFile(filepath.Join(remoteDir, "zero-memcp.lock.pid"), []byte("4194303\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := gitstore.Init(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "a.md", []byte("x\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := PushLocal(repo, remoteDir, "zero"); err != nil {
		t.Fatalf("残留锁应被接管: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteDir, "zero-memcp.lock.pid")); !os.IsNotExist(err) {
		t.Fatal("推送结束后锁文件应被删除")
	}
}

// 活着的进程持锁时必须等不到就报错，而不是无限排队。
func TestLiveLockBlocksWithTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "zero-memcp.lock.pid"), []byte(itoaLocal(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := lockLocalRepo(dir, 120*time.Millisecond)
	if err == nil {
		t.Fatal("活锁必须失败")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("等锁必须有超时上限")
	}
}

func itoaLocal(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
