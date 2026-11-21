package gitstore

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSyncFileOnReadOnlyOpenedHandle 钉住一个只在 Windows 上显形的 bug。
//
// 故障现场（2026-11-21，3060 Windows 11 真机）：
//
//	dirty sync BlackEggAI/zero: sync …\dirty\meta.index\@.dirty: Access is denied.
//
// 成因：syncFile 用 os.Open（只读句柄）打开文件再调 f.Sync()。Unix 上只读
// fd 可以 fsync，但 Windows 的 FlushFileBuffers 要求句柄带 GENERIC_WRITE，
// 只读句柄直接返回 ERROR_ACCESS_DENIED。于是「刷盘」这个动作在 Windows 上
// 从未成功过——而它恰好是数据落盘保证的最后一环。
//
// 为什么只有 meta.index 报出来：主记忆的脏文件写完很快被 commit 清掉，
// 刷盘来不及撞上；索引元数据长期滞留在脏层，每轮刷盘都撞。
func TestSyncFileOnReadOnlyOpenedHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload")

	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	if err := syncFile(path); err != nil {
		t.Fatalf("syncFile 失败: %v\n"+
			"这是数据落盘链路的最后一环，失败意味着「写入已持久化」的承诺不成立", err)
	}
}

// TestSyncFileMissingIsNotAnError 不存在的文件不该判失败。
//
// 提交会清空脏层目录，刷盘可能晚一步跑到；对已消失的文件报错，
// 等于让一次正常的清理把整批刷盘判失败。
func TestSyncFileMissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := syncFile(filepath.Join(dir, "gone")); err != nil {
		t.Fatalf("文件不存在时 syncFile 应返回 nil，实际: %v", err)
	}
}

// TestSyncFileDirtyFileNameIsWritable 确认脏层命名在文件系统层面可用。
//
// 脏层文件名规则是 dirtyPrefix("@"）+ 树内目录 + dirtySuffix(".dirty")，
// 根目录为 "@.dirty"。这两个字符在 Windows 上都是合法文件名字符——
// 曾经怀疑是 "@" 或 "." 导致 Access denied，实测证明不是（见上面的
// TestSyncFileOnReadOnlyOpenedHandle）。这条测试把这个结论钉住，
// 免得以后有人把病因归到命名上、改错地方。
func TestSyncFileDirtyFileNameIsWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dirtyPrefix+dirtySuffix) // "@.dirty"
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("写 @.dirty 失败: %v", err)
	}
	if err := syncFile(path); err != nil {
		t.Fatalf("sync @.dirty 失败: %v", err)
	}

	nested := filepath.Join(dir, "meta.index", dirtyPrefix+dirtySuffix)
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(nested, []byte("x"), 0o644); err != nil {
		t.Fatalf("写 meta.index/@.dirty 失败: %v", err)
	}
	if err := syncFile(nested); err != nil {
		t.Fatalf("sync meta.index/@.dirty 失败: %v", err)
	}
}
