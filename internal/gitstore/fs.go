package gitstore

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ReaderAtCloser 组合顺序读、随机读与关闭，供 packfile 按偏移检索。
type ReaderAtCloser interface {
	io.ReaderAt
	io.Reader
	io.Seeker
	io.Closer
}

// FS 抽象仓库的全部落盘动作。
//
// 为什么要有这一层：本地裸目录只是当前实现，而记忆底座最终要能在
// 共享块设备、对象存储适配层、乃至纯内存测试环境上跑同一套 Git 语义。
// 接口里刻意区分三种写，因为它们的**承诺不同**，不可合并：
//
//	WriteFile    直接覆写。读者可能看见半截，只用于一次性建档。
//	WriteAtomic  临时文件 + rename，不 fsync。读者只会看到完整旧版或
//	             完整新版；掉电最多退回上一版。ref、对象、脏文件用它。
//	WriteDurable 临时文件 + fsync + rename + 目录 fsync。用于 pack/idx
//	             这类「事后不会再重算、丢了就得重新拉」的派生物。
//
// 区分这三档是有代价的：接口的调用者必须知道自己需要哪一档承诺。
// 但把三档压成一个 "Write"，要么在热路径上白付 fsync 的钱，
// 要么在最需要耐久性的地方悄悄丢数据。
type FS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	WriteAtomic(path string, data []byte, perm os.FileMode) error
	// WriteAtomicSync 在原子 rename 前 fsync 临时文件，但不 fsync 目录。
	WriteAtomicSync(path string, data []byte, perm os.FileMode) error

	// WriteAtomicWith 是 WriteAtomic 的流式版本：由 FS 负责临时文件的
	// 生命周期与 rename，调用方负责往 writer 里灌字节。
	// 松散对象要 zlib 压缩，只有拿到 io.Writer 才能边压边写，
	// 不该为了走接口而先把整对象压在内存里。
	WriteAtomicWith(path string, perm os.FileMode, fill func(io.Writer) error) error
	// WriteAtomicWithSync 是流式原子写入，rename 前 fsync 临时文件。
	WriteAtomicWithSync(path string, perm os.FileMode, fill func(io.Writer) error) error

	WriteDurable(path string, data []byte, perm os.FileMode) error

	Remove(path string) error
	RemoveAll(path string) error
	Stat(path string) (os.FileInfo, error)
	MkdirAll(path string, perm os.FileMode) error
	ReadDir(path string) ([]os.DirEntry, error)
	WalkDir(root string, fn fs.WalkDirFunc) error

	// Open 打开文件用于流式读取。返回的接口带 Seek：
	// packfile 读取要先定位到对象偏移再解压，只给 io.Reader 做不到。
	Open(path string) (ReaderAtCloser, error)
}

// osFS 是直接基于操作系统文件系统的原生实现。
type osFS struct{}

func DefaultFS() FS { return osFS{} }

func (osFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (osFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (osFS) WriteAtomic(path string, data []byte, perm os.FileMode) error {
	return writeAtomicNative(path, perm, false, false, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func (osFS) WriteAtomicSync(path string, data []byte, perm os.FileMode) error {
	return writeAtomicNative(path, perm, true, false, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func (osFS) WriteAtomicWith(path string, perm os.FileMode, fill func(io.Writer) error) error {
	return writeAtomicNative(path, perm, false, false, fill)
}

func (osFS) WriteAtomicWithSync(path string, perm os.FileMode, fill func(io.Writer) error) error {
	return writeAtomicNative(path, perm, true, false, fill)
}

func (osFS) WriteDurable(path string, data []byte, perm os.FileMode) error {
	return writeAtomicNative(path, perm, true, true, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func writeAtomicNative(path string, perm os.FileMode, syncFile, syncDirectory bool, fill func(io.Writer) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	// 保持调用方要求的权限位；沿用原子写实现的语义，chmod 失败不能静默降级。
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}

	if err := fill(tmp); err != nil {
		tmp.Close()
		return err
	}
	if syncFile {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if syncDirectory {
		return syncDir(dir)
	}
	return nil
}

// syncDir 在 Unix 上 fsync 目录；Windows 上目录不能 fsync，退化为空操作。
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (osFS) Remove(path string) error {
	return os.Remove(path)
}

func (osFS) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (osFS) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func (osFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (osFS) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (osFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	return filepath.WalkDir(root, fn)
}

func (osFS) Open(path string) (ReaderAtCloser, error) {
	return os.Open(path)
}

// ── 锁抽象 ────────────────────────────────────────────────────

// Locker 抽象仓库运行期的并发同步与互斥机制。
//
// 对应三层边界：
//  1. 脏目录锁：同目录串行重写，不同目录并发；
//  2. 脏层全局读写锁：改脏 RLock，折叠 Commit 全局独占 Lock；
//  3. 分支提交令牌：同一分支只能有一个提交在进行，带超时。
type Locker interface {
	// LockNode 获取某分支某脏目录的文件级锁。
	LockNode(ref, dir string) (unlock func())

	// RLockDirty 获取脏层全局读锁（写入与只读覆盖）。
	RLockDirty() (unlock func())

	// LockDirty 获取脏层全局写锁（提交折叠）。
	LockDirty() (unlock func())

	// AcquireCommitToken 获取某分支的提交互斥令牌。
	AcquireCommitToken(ref string, timeout time.Duration) (release func(), err error)
}

// localLocker 是基于内存 sync.RWMutex 与 sync.Map 的原生单进程锁实现。
type localLocker struct {
	dirtyMu   sync.RWMutex
	nodeLocks sync.Map // normRef+"\x00"+dir -> *sync.Mutex
	commitTok sync.Map // normRef -> chan struct{} (cap 1)
}

func newLocalLocker() *localLocker {
	return &localLocker{}
}

func (l *localLocker) LockNode(ref, dir string) func() {
	key := normalizeRef(ref) + "\x00" + dir
	v, _ := l.nodeLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (l *localLocker) RLockDirty() func() {
	l.dirtyMu.RLock()
	return l.dirtyMu.RUnlock
}

func (l *localLocker) LockDirty() func() {
	l.dirtyMu.Lock()
	return l.dirtyMu.Unlock
}

func (l *localLocker) AcquireCommitToken(normRef string, timeout time.Duration) (func(), error) {
	v, _ := l.commitTok.LoadOrStore(normRef, make(chan struct{}, 1))
	ch := v.(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	select {
	case ch <- struct{}{}:
		var releaseOnce sync.Once
		return func() {
			releaseOnce.Do(func() { <-ch })
		}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("分支 %s 提交排队超时（上一个提交占用超过 %v）", normRef, timeout)
	}
}
