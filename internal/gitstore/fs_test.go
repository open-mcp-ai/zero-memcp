package gitstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// spyFS 包装默认 FS，记录关键操作的调用频次，验证 Repo 确实在通过接口路由 I/O。
type spyFS struct {
	FS
	readCount    atomic.Int64
	writeCount   atomic.Int64
	appendCount  atomic.Int64
	durableCount atomic.Int64
}

func (s *spyFS) Append(path string, data []byte, perm os.FileMode) error {
	s.appendCount.Add(1)
	return s.FS.Append(path, data, perm)
}

func (s *spyFS) WriteDurable(path string, data []byte, perm os.FileMode) error {
	s.durableCount.Add(1)
	return s.FS.WriteDurable(path, data, perm)
}

func (s *spyFS) ReadFile(path string) ([]byte, error) {
	s.readCount.Add(1)
	return s.FS.ReadFile(path)
}

func (s *spyFS) WriteAtomic(path string, data []byte, perm os.FileMode) error {
	s.writeCount.Add(1)
	return s.FS.WriteAtomic(path, data, perm)
}

func (s *spyFS) WriteAtomicSync(path string, data []byte, perm os.FileMode) error {
	s.writeCount.Add(1)
	return s.FS.WriteAtomicSync(path, data, perm)
}

// 验证可通过 InitWith 注入自定义存储并拦截脏层与 ref 写入。
func TestRepoInjectFS(t *testing.T) {
	dir := t.TempDir()
	spy := &spyFS{FS: DefaultFS()}
	r, err := InitWith(filepath.Join(dir, "spy.git"), spy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// 1. 脏写入
	if err := r.Write("zero", "notes/today.md", []byte("hello"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if spy.writeCount.Load() == 0 {
		t.Fatalf("脏层写入应路由至自定义 FS，但 writeCount = 0")
	}

	// 2. 读脏层
	data, err := r.Read("zero", "notes/today.md")
	if err != nil || string(data) != "hello" {
		t.Fatalf("通过自定义 FS 读取内容异常: %v, %s", err, string(data))
	}
	if spy.readCount.Load() == 0 {
		t.Fatalf("脏层读取应路由至自定义 FS，但 readCount = 0")
	}

	// 3. Commit
	sha, err := r.Commit("zero", "first")
	if err != nil || sha == "" {
		t.Fatalf("Commit 失败: %v", err)
	}
}

type spyLocker struct {
	Locker
	lockNodeCalls atomic.Int64
	commitTokens  atomic.Int64
}

func (s *spyLocker) LockNode(ref, dir string) (func(), error) {
	s.lockNodeCalls.Add(1)
	return s.Locker.LockNode(ref, dir)
}

func (s *spyLocker) AcquireCommitToken(ref string, timeout time.Duration) (func(), error) {
	s.commitTokens.Add(1)
	return s.Locker.AcquireCommitToken(ref, timeout)
}

func TestRepoInjectLocker(t *testing.T) {
	dir := t.TempDir()
	spy := &spyLocker{Locker: newLocalLocker()}
	r, err := InitWith(filepath.Join(dir, "lockspy.git"), nil, spy)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("zero", "a.md", []byte("A"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if spy.lockNodeCalls.Load() == 0 {
		t.Fatalf("目录锁未路由至自定义 Locker: %d", spy.lockNodeCalls.Load())
	}

	if _, err := r.Commit("zero", "msg"); err != nil {
		t.Fatal(err)
	}
	if spy.commitTokens.Load() == 0 {
		t.Fatalf("提交令牌未路由至自定义 Locker: %d", spy.commitTokens.Load())
	}
}

// 验证 isNotExist 能解包 fmt.Errorf("%w")，对自定义 FS 友好。
func TestIsNotExistUnwrapsWrappedError(t *testing.T) {
	wrapped := fmt.Errorf("storage layer error: %w", os.ErrNotExist)
	if !isNotExist(wrapped) {
		t.Fatalf("应该识别包装过的 os.ErrNotExist")
	}
	doubleWrapped := fmt.Errorf("remote: %w", fmt.Errorf("inner: %w", os.ErrNotExist))
	if !isNotExist(doubleWrapped) {
		t.Fatalf("应该识别多层包装的 os.ErrNotExist")
	}
	if isNotExist(fmt.Errorf("some other error")) {
		t.Fatalf("不相关错误不应判为 NotExist")
	}
}

// 写入脏层走延迟批量 fsync：写时只写页缓存（0.5ms），
// SyncDirtyNow 负责把待刷盘集合批量 fsync。
// 理由：用户定调「fsync 自动、定时或延迟做一下，不用每写必刷」。
func TestDirtyLayerSyncsInBatch(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 写两条：进脏层、进待刷盘集合，但写入路径本身不硬等 fsync
	if err := r.Write("main", "a/1.md", []byte("1"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "a/2.md", []byte("2"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Barrier(); err != nil {
		t.Fatal(err)
	}

	// 此时待刷盘集合里必须有这个目录的脏文件
	pendingCount := 0
	r.dirtySyncPending.Range(func(_, _ any) bool {
		pendingCount++
		return true
	})
	if pendingCount == 0 {
		t.Fatal("写入后 dirtySyncPending 为空，说明没有登记待刷盘")
	}

	// 调一次批量刷盘：必须清空集合
	if err := r.SyncDirtyNow(); err != nil {
		t.Fatalf("SyncDirtyNow: %v", err)
	}
	afterCount := 0
	r.dirtySyncPending.Range(func(_, _ any) bool {
		afterCount++
		return true
	})
	if afterCount != 0 {
		t.Fatalf("SyncDirtyNow 后待刷盘集合未清空，剩 %d 项", afterCount)
	}
}
