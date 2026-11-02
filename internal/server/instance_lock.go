package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// instanceLock 是数据目录级跨进程独占锁。
//
// Locker（脏目录锁、脏层 RW 锁、分支提交令牌）只在进程内。
// 若两个服务实例同时打开一个 dataDir，它们都以为自己独占仓库，
// 会同时折叠同一条分支，提交一个从未存在过的 tree。
//
// 锁用操作系统文件锁：进程崩溃时内核自动释放，不需要猜 PID 是否存活，
// 不需要删 lock 文件，也没有「两个进程同时清理残留锁」的竞争窗口。
//
// 所有写者都必须持锁：服务持锁整个生命周期；GC 已迁进 WebUI（同进程），
// standalone maintenance CLI 不再存在。纯只读的外部 git clone 不受影响。
type instanceLock struct {
	path string
	file *os.File
	mu   sync.Mutex
}

func instanceLockPath(dataDir string) string {
	return filepath.Join(dataDir, "system", "instance.lock")
}

func acquireInstanceLock(dataDir string) (*instanceLock, error) {
	path := instanceLockPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create directory %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock %s: %w", path, err)
	}
	if err := lockOSFile(f); err != nil {
		_ = f.Close()
		pid := readLockPID(path)
		if pid > 0 {
			return nil, fmt.Errorf("data directory %s is in use by process %d.\n"+
				"  only one writer may use a directory; simultaneous commits or object packing by two instances can corrupt it",
				dataDir, pid)
		}
		return nil, fmt.Errorf("data directory %s is in use by another process; only one writer may use a directory", dataDir)
	}
	// 持锁以后再覆盖 PID。锁文件永久保留，锁状态由内核维护，
	// 所以 Release 不删除文件（删除会产生 inode 替换竞态）。
	if err := f.Truncate(0); err != nil {
		_ = unlockOSFile(f)
		_ = f.Close()
		return nil, fmt.Errorf("truncate instance lock: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = unlockOSFile(f)
		_ = f.Close()
		return nil, fmt.Errorf("seek instance lock: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		_ = unlockOSFile(f)
		_ = f.Close()
		return nil, fmt.Errorf("write instance lock: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = unlockOSFile(f)
		_ = f.Close()
		return nil, fmt.Errorf("sync instance lock: %w", err)
	}
	return &instanceLock{path: path, file: f}, nil
}

func readLockPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func (l *instanceLock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	unlockErr := unlockOSFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
