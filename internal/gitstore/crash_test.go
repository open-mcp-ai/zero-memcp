package gitstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 断电时日志最后一行只写了一半：重放必须丢弃它，绝不能让半行
// 和后面的记录串在一起，把两条正常记录一起毁掉。
func TestWALTruncatedTailDoesNotCorrupt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trunc.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "a.md", []byte("第一条\n"), ModeContent); err != nil {
		t.Fatal(err)
	}

	// 手工追加一个被截断的半行（模拟写到一半断电）
	f, err := os.OpenFile(r.walPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("deadbeef\tzero\t100644\tabc") // 没有 \n
	f.Close()

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 完整的那条必须还在
	if got, err := r2.Read("zero", "a.md"); err != nil || string(got) != "第一条\n" {
		t.Fatalf("截断尾不该影响已写入的记录: %v", err)
	}
	// 半行不该被当成有效记录
	if n := r2.Pending("zero"); n != 1 {
		t.Fatalf("pending 应为 1，得到 %d", n)
	}

	// 再写一条：不能与半行粘连
	if err := r2.Write("zero", "b.md", []byte("第二条\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r3.Read("zero", "b.md"); err != nil || string(got) != "第二条\n" {
		t.Fatalf("半行之后的记录必须完好: %v", err)
	}
}

// 日志中间被写坏（位翻转）：坏行之后一律不信任，但坏的之前必须全在。
func TestWALCorruptLineStopsReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "corrupt.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.md", "b.md", "c.md"} {
		if err := r.Write("zero", n, []byte("内容"+n+"\n"), ModeContent); err != nil {
			t.Fatal(err)
		}
	}

	// 把第二行的校验码改掉
	b, err := os.ReadFile(r.walPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 3 {
		t.Fatalf("日志应有至少 3 行，得到 %d", len(lines))
	}
	lines[1] = "00000000" + lines[1][8:]
	os.WriteFile(r.walPath(), []byte(strings.Join(lines, "\n")), 0o644)

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 第一条在坏行之前，必须恢复
	if got, err := r2.Read("zero", "a.md"); err != nil || !strings.Contains(string(got), "内容a.md") {
		t.Fatalf("坏行之前的记录应恢复: %v", err)
	}
	// 坏行及其之后不可信
	if n := r2.Pending("zero"); n != 1 {
		t.Fatalf("坏行之后应停止重放，pending 应为 1，得到 %d", n)
	}
}

// 两个分支写同一个路径：必须各存各的，提交互不干扰。
func TestTwoBranchesSamePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "branch.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Write("chat", "same.md", []byte("chat 的内容\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("dev", "same.md", []byte("dev 的内容\n"), ModeContent); err != nil {
		t.Fatal(err)
	}

	// 提交前：各自读到自己那份
	if got, err := r.Read("chat", "same.md"); err != nil || string(got) != "chat 的内容\n" {
		t.Fatalf("chat 分支读错: %q, %v", got, err)
	}
	if got, err := r.Read("dev", "same.md"); err != nil || string(got) != "dev 的内容\n" {
		t.Fatalf("dev 分支读错: %q, %v", got, err)
	}

	// 只提交 chat
	if _, err := r.Commit("chat", "只提交 chat"); err != nil {
		t.Fatal(err)
	}

	// 重启：dev 的未提交内容必须还在
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r2.Read("chat", "same.md"); err != nil || string(got) != "chat 的内容\n" {
		t.Fatalf("chat 已提交的内容丢了: %q, %v", got, err)
	}
	if got, err := r2.Read("dev", "same.md"); err != nil || string(got) != "dev 的内容\n" {
		t.Fatalf("dev 未提交的内容丢了: %q, %v", got, err)
	}

	// 再提交 dev
	if _, err := r2.Commit("dev", "再提交 dev"); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r3.Read("dev", "same.md"); string(got) != "dev 的内容\n" {
		t.Fatalf("dev 提交后内容错: %q", got)
	}
	// 两个分支现在都进了各自的树
	for _, br := range []string{"chat", "dev"} {
		list, err := r3.List(br)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Path != "same.md" {
			t.Fatalf("%s 分支的树不对: %+v", br, list)
		}
	}
}

// blob 已经落盘但索引行没写成：内容成为孤儿，但下次重写同内容
// 会命中同一个 SHA——所以这种半成功不会导致内容被篡改或重复。
func TestOrphanBlobIsHarmless(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "orphan.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("孤儿内容\n")
	// 只写 blob，不写索引行
	sha, err := r.writeObject("blob", data)
	if err != nil {
		t.Fatal(err)
	}

	// 重启：孤儿不该出现在任何分支里
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := r2.Pending(); n != 0 {
		t.Fatalf("孤儿 blob 不该变成待提交项，得到 %d", n)
	}

	// 正常写入同样的内容：拿到同一个 SHA，不重复占空间
	if err := r2.Write("zero", "x.md", data, ModeContent); err != nil {
		t.Fatal(err)
	}
	if got, _ := r2.BlobHash(data); got != sha {
		t.Fatalf("同一内容应得到同一 SHA：%s vs %s", got, sha)
	}
}
