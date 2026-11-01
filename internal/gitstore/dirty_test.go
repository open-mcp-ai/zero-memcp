package gitstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 写进脏层，在 Commit 前必须能被 Read 和 List 读到；Commit 之后折叠进正式树。
func TestDirtyOverlayWriteReadAndCommit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "overlay.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 1. 写进不同深度的子目录
	if err := r.Write("main", "root.md", []byte("根文件\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "docs/intro.md", []byte("文档介绍\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "a/b/c.md", []byte("深层文件\n"), ModeContent); err != nil {
		t.Fatal(err)
	}

	// 2. 检查待提交数量
	if n := r.Pending("main"); n != 3 {
		t.Fatalf("pending 应为 3，得到 %d", n)
	}

	// 3. 提交前即可读出内容
	for path, want := range map[string]string{
		"root.md":       "根文件\n",
		"docs/intro.md": "文档介绍\n",
		"a/b/c.md":      "深层文件\n",
	} {
		got, err := r.Read("main", path)
		if err != nil || string(got) != want {
			t.Fatalf("读 %s 失败: err=%v, got=%q, want=%q", path, err, string(got), want)
		}
	}

	// 4. List 应列出全部 3 篇
	entries, err := r.List("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("List 应列出 3 项，得到 %d", len(entries))
	}

	// 5. Commit 提交
	csha, err := r.Commit("main", "首次提交")
	if err != nil || csha == "" {
		t.Fatalf("Commit 失败: %v, csha=%s", err, csha)
	}

	// 6. 提交后 pending 必须归零，脏文件必须被清理
	if n := r.Pending("main"); n != 0 {
		t.Fatalf("提交后 pending 应为 0，得到 %d", n)
	}
	dirtyFiles, _ := r.listDirtyDirs("main")
	if len(dirtyFiles) != 0 {
		t.Fatalf("提交后脏目录应被清空，得到 %v", dirtyFiles)
	}

	// 7. 正式树依然完整可读
	for path, want := range map[string]string{
		"root.md":       "根文件\n",
		"docs/intro.md": "文档介绍\n",
		"a/b/c.md":      "深层文件\n",
	} {
		got, err := r.Read("main", path)
		if err != nil || string(got) != want {
			t.Fatalf("提交后读 %s 失败: err=%v, got=%q", path, err, string(got))
		}
	}
}

// 模拟进程被强杀：写了但未 Commit，重新打开仓库依然完好恢复。
func TestDirtyRecoversUncommittedWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "crash.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 写一条，故意不 Commit（模拟此时进程强杀）
	if err := r.Write("zero", "survive.md", []byte("脏文件不丢\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "sub/tree.md", []byte("子目录不丢\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	// put 的确认只代表已入队；这里显式等待写者，以模拟「已刷盘后进程被杀」。
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}

	// 重新打开（模拟重启）
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 验证 pending 状态与内容依然存在
	if n := r2.Pending("zero"); n != 2 {
		t.Fatalf("重启后 pending 应为 2，得到 %d", n)
	}
	got, err := r2.Read("zero", "survive.md")
	if err != nil || string(got) != "脏文件不丢\n" {
		t.Fatalf("重启后未恢复脏内容: err=%v, got=%q", err, string(got))
	}
	gotSub, err := r2.Read("zero", "sub/tree.md")
	if err != nil || string(gotSub) != "子目录不丢\n" {
		t.Fatalf("重启后未恢复子目录脏内容: err=%v, got=%q", err, string(gotSub))
	}

	// 提交并验证
	sha, err := r2.Commit("zero", "恢复后提交")
	if err != nil || sha == "" {
		t.Fatalf("提交失败: %v", err)
	}
	if n := r2.Pending("zero"); n != 0 {
		t.Fatalf("提交后 pending 应为 0，得到 %d", n)
	}
}

// 脏文件末尾被截断（模拟断电只写了一半）：解析器必须跳过坏行，不崩溃。
func TestDirtyCorruptTailDoesNotCrash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trunc.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "a.md", []byte("第一条\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}

	// 往脏文件追加半截垃圾行
	p := r.dirtyFilePath("zero", "")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("S\t100644\tdeadbeef\t") // 半截无换行
	f.Close()

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 合法的记录依然可读
	got, err := r2.Read("zero", "a.md")
	if err != nil || string(got) != "第一条\n" {
		t.Fatalf("半截尾部不该破坏正常记录: %v, got=%q", err, string(got))
	}
}

// 删除操作（墓碑）：在脏层打墓碑，提交时从 Git 树里彻底移除。
func TestDirtyDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "del.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 先写并提交
	if err := r.Write("main", "a.md", []byte("甲\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "b.md", []byte("乙\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "初始"); err != nil {
		t.Fatal(err)
	}

	// 删除 a.md
	if err := r.Delete("main", "a.md"); err != nil {
		t.Fatal(err)
	}

	// 提交前 Read a.md 必须报不存在
	if _, err := r.Read("main", "a.md"); err == nil {
		t.Fatal("删除后应读不到 a.md")
	}
	// b.md 依然正常
	if got, err := r.Read("main", "b.md"); err != nil || string(got) != "乙\n" {
		t.Fatalf("b.md 应完好: %v", err)
	}

	// Deleted() 应包含 a.md
	del := r.Deleted("main")
	if len(del) != 1 || del[0] != "a.md" {
		t.Fatalf("Deleted 应包含 a.md，得到 %v", del)
	}

	// 提交后验证
	if _, err := r.Commit("main", "删除 a"); err != nil {
		t.Fatal(err)
	}
	entries, err := r.List("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "b.md" {
		t.Fatalf("提交后列表应只剩 b.md，得到 %v", entries)
	}
}

// 两个分支写同一个路径：必须各存各的脏文件，互不干扰。
func TestDirtyTwoBranchesSamePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "branch.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Write("chat", "same.md", []byte("chat 内容\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("dev", "same.md", []byte("dev 内容\n"), ModeContent); err != nil {
		t.Fatal(err)
	}

	// 各自读到自己那份
	if got, err := r.Read("chat", "same.md"); err != nil || string(got) != "chat 内容\n" {
		t.Fatalf("chat 分支读错: %q", got)
	}
	if got, err := r.Read("dev", "same.md"); err != nil || string(got) != "dev 内容\n" {
		t.Fatalf("dev 分支读错: %q", got)
	}

	// 只提交 chat
	if _, err := r.Commit("chat", "只提交 chat"); err != nil {
		t.Fatal(err)
	}

	// dev 依然是待提交脏状态
	if n := r.Pending("dev"); n != 1 {
		t.Fatalf("dev pending 应为 1，得到 %d", n)
	}
	if got, err := r.Read("dev", "same.md"); err != nil || string(got) != "dev 内容\n" {
		t.Fatalf("dev 分支内容丢失: %q", got)
	}
}

// 多 goroutine 并发写不同目录与同一目录，一条都不能丢。
func TestDirtyConcurrentWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "concur.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	const W = 6
	const N = 20
	var wg sync.WaitGroup
	for w := 0; w < W; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < N; i++ {
				// 既有同目录竞争（dir0），也有各自独立目录（dirW）
				p1 := fmt.Sprintf("shared/w%d_%d.md", worker, i)
				p2 := fmt.Sprintf("dir%d/f_%d.md", worker, i)
				if err := r.Write("main", p1, []byte("x"), ModeContent); err != nil {
					t.Errorf("write %s: %v", p1, err)
					return
				}
				if err := r.Write("main", p2, []byte("y"), ModeContent); err != nil {
					t.Errorf("write %s: %v", p2, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if n := r.Pending("main"); n != W*N*2 {
		t.Fatalf("pending 应为 %d，得到 %d", W*N*2, n)
	}

	// 提交
	sha, err := r.Commit("main", "并发写入提交")
	if err != nil || sha == "" {
		t.Fatalf("Commit 失败: %v", err)
	}

	entries, err := r.List("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != W*N*2 {
		t.Fatalf("最终条目数应为 %d，实际 %d", W*N*2, len(entries))
	}
}
