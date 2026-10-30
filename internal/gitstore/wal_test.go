package gitstore

import (
	"path/filepath"
	"testing"
)

// 验证 kill -9 不丢内容：写了但没 Commit 就退出，重新打开依然能读。
func TestWALRecoversUncommittedWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wal.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 写一条，故意不调用 Commit() —— 模拟进程此时被 kill -9
	if err := r.Write("zero", "survive.md", []byte("断电不丢\n"), ModeContent); err != nil {
		t.Fatal(err)
	}

	// 重新 Open 一个新实例（模拟进程重启）
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 此时它应该通过 ReplayWAL 恢复了待提交区，且内容可读
	got, err := r2.Read("zero", "survive.md")
	if err != nil || string(got) != "断电不丢\n" {
		t.Fatalf("重启后未恢复 WAL 内容: err=%v, got=%q", err, string(got))
	}

	// 正常提交，验证可以顺利封存进树
	sha, err := r2.Commit("zero", "recovered")
	if err != nil || sha == "" {
		t.Fatalf("恢复后的条目应能正常提交: %v", err)
	}

	// 提交后 WAL 应被清空
	r3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := r3.Pending(); n != 0 {
		t.Fatalf("提交后 pending 应归零，得到 %d", n)
	}
	// 且从正式树里能读到
	got, err = r3.Read("zero", "survive.md")
	if err != nil || string(got) != "断电不丢\n" {
		t.Fatalf("提交后正式树里应能读到: %v", err)
	}
}
