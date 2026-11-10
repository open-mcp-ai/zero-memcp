//go:build !windows

package daemon

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Detach 让子进程成为独立的 session leader，即使父进程退出也继续运行，
// 且脱离控制终端。
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// ProcessAlive 报告 pid 是否对应一个存活进程。EPERM 表示存在但属主不同。
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// terminate 发送 SIGTERM。zero-memcp 的 signal handler 会执行优雅退出。
func terminate(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}
