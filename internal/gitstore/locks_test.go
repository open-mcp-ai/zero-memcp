package gitstore

import (
	"path/filepath"
	"testing"
)

// 锁表缓存的正确性：加锁/解锁之后，判定结果必须立刻跟着变。
//
// 缓存的收益是省掉热路径上一次真实磁盘读（见 locks.go 的注释），
// 但锁是权限判定的输入——缓存错了就是越权。所以两个失效点都必须有测试。
func TestBranchLockedCacheInvalidatedByLockUnlock(t *testing.T) {
	dir := t.TempDir()
	repo, err := Init(filepath.Join(dir, "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main", "a.md", []byte("x"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("dev", "a.md", []byte("x"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("dev", "seed"); err != nil {
		t.Fatal(err)
	}

	// 先查一次，把「都未锁」这份结果灌进缓存。
	if locked, _ := repo.BranchLocked("main"); locked {
		t.Fatal("起点不应被锁")
	}
	if locked, _ := repo.BranchLocked("dev"); locked {
		t.Fatal("起点不应被锁")
	}

	// 加锁后必须立刻可见——这正是缓存最容易出错的地方。
	if _, err := repo.LockBranch("main"); err != nil {
		t.Fatal(err)
	}
	if locked, sha := repo.BranchLocked("main"); !locked || sha == "" {
		t.Fatal("刚加锁的分支查不到锁：解锁/加锁没有让缓存失效")
	}
	// 其它分支不能因此被误判成锁住。
	if locked, _ := repo.BranchLocked("dev"); locked {
		t.Fatal("加锁 main 之后 dev 被误判为锁住")
	}

	// 解锁后同理。
	if ok, err := repo.UnlockBranch("main"); err != nil || !ok {
		t.Fatalf("解锁失败: ok=%v err=%v", ok, err)
	}
	if locked, _ := repo.BranchLocked("main"); locked {
		t.Fatal("刚解锁的分支仍报锁住：解锁没有让缓存失效")
	}
}

// 损坏的锁文件不能把分支永久封死。
//
// 锁是「额外的拒绝理由」，不是权限本身——一个写坏的元数据文件
// 不应该比没有锁更糟。旧实现逐分支读文件时就是这个行为，缓存路径
// 必须保持一致。
func TestBranchLockedIgnoresCorruptLockFile(t *testing.T) {
	dir := t.TempDir()
	repo, err := Init(filepath.Join(dir, "r.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main", "a.md", []byte("x"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main", "seed"); err != nil {
		t.Fatal(err)
	}

	// 直接写一个内容不是 SHA 的锁文件。
	p := repo.lockPath(normalizeRef("main"))
	if err := repo.FS().MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := repo.FS().WriteAtomic(p, []byte("not-a-sha\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if locked, _ := repo.BranchLocked("main"); locked {
		t.Fatal("内容不是 SHA 的锁文件应当被忽略，而不是把分支锁死")
	}
}
