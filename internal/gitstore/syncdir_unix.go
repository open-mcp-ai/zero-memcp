//go:build !windows

package gitstore

import "os"

// syncDir 让目录项落盘（保证刚 rename 进来的文件名断电后还在）。
//
// 只有 Unix 需要也支持这一步：目录本身是一个 fd，fsync 它会同步目录内容。
// Windows 走 syncDirWindows（见 syncdir_windows.go）。
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		// 目录已经不存在 = 没有目录项需要同步。
		//
		// 这不是理论情况：批量刷盘（SyncDirtyNow）可能在提交刚清空
		// 工作层目录之后才跑到，此时那个目录已经没了。对不存在的目录
		// 报错，等于让一次正常的清理把刷盘整体判失败。
		if isNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return f.Sync()
}
