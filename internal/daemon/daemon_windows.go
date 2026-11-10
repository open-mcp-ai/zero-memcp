//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive 是 Windows 表示进程仍在运行的退出码。
const stillActive = 259

// Detach 让子进程不创建控制台窗口、独立进程组，从而不受父进程控制台事件
// 影响，并在父进程退出后存活。
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

// ProcessAlive 报告 pid 是否存活。拒绝访问同样证明进程存在（属主不同）。
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// terminate 强制终止进程。
//
// Windows 上没有能送达 detached 进程的信号（控制台事件需要共享控制台），
// 所以这里是硬杀而不是优雅退出。这对 zero-memcp 安全：Git 对象库本身
// 不会写坏，脏层未折叠的内容留在磁盘上，下次启动重新加载。
func terminate(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminate process %d: %w", pid, err)
	}
	return nil
}
