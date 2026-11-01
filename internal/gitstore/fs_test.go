package gitstore

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// spyFS 包装默认 FS，记录关键操作的调用频次，验证 Repo 确实在通过接口路由 I/O。
type spyFS struct {
	FS
	readCount  atomic.Int64
	writeCount atomic.Int64
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

func (s *spyLocker) LockNode(ref, dir string) func() {
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
