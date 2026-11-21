//go:build windows

package gitstore

import "syscall"

// syncDir 在 Windows 上刷目录。
//
// 为什么不能照搬 Unix 的 os.Open + Sync：Windows 上目录不是普通文件，
// os.Open 拿到的是只读句柄，FlushFileBuffers 要求 GENERIC_WRITE，
// 于是直接 ERROR_ACCESS_DENIED。而且打开目录本身必须带
// FILE_FLAG_BACKUP_SEMANTICS，否则 CreateFile 根本不让你打开目录。
//
// 语义上的取舍：NTFS 是元数据日志文件系统，rename 返回成功即已进入
// 日志，断电后目录项不会丢。所以这里刷目录是「锦上添花」，不是
// 数据可见性的必要条件——刷不动时不该让整批工作层刷盘判失败。
// 这与 Unix 版（刷不动就是错）的差别来自文件系统保证本身不同，
// 不是放宽标准。
func syncDir(dir string) error {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return nil
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		// 目录不存在、或该文件系统不提供可写目录句柄：都不是刷盘失败。
		return nil
	}
	defer syscall.CloseHandle(h)

	// 某些文件系统对目录句柄的 FlushFileBuffers 返回
	// ERROR_INVALID_FUNCTION / ERROR_ACCESS_DENIED。原因见上面的 NTFS 说明。
	_ = syscall.FlushFileBuffers(h)
	return nil
}
