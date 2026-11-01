//go:build !windows

package gittransport

import "syscall"

// processAlive 报告 pid 对应的进程是否还活着。
//
// 0 号信号不改任何状态，只做权限与存在性检查——这是判断
// 「锁文件的主人还在不在」最轻的办法。EPERM 说明进程存在但不属于我，
// 那也算活着：不能因为读不到就抢锁。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
