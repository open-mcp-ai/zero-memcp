//go:build windows

package gittransport

// processAlive 在 Windows 上一律当作「活着」。
//
// 判定存活要靠 OpenProcess，而拿不到句柄时分不清「进程不存在」和
// 「没权限」。猜错会去抢别人正在持有的锁，所以宁可不抢：
// 代价是一个崩溃残留的锁会让推送在超时后报错，等人去删文件。
func processAlive(int) bool { return true }
