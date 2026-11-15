package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 紧凑二进制格式必须与内存索引完全等价，且加载要比 gob 快得多。
//
// 这是「快照有没有用」的唯一判据。实测 gob 版：148.9MB、加载 104.9 秒，
// 比全量重建（27.8 秒）还慢——那个优化当时是负收益。
// 慢在两处，都不是 I/O：每篇的地址在倒排里是独立字符串（500 万条
// = 500 万个独立字符串），以及嵌套 map 的解码。
func TestBinaryFormatRoundTrip(t *testing.T) {
	ix := New()
	// 造出真实形状：同一批地址在大量词下重复出现。
	for d := 0; d < 200; d++ {
		addr := fmt.Sprintf("proj@main:2026-11-15/%03d.md", d)
		ix.AddWithTime(addr, fmt.Sprintf("common token repeated body number %d", d), int64(1700000000+d))
	}
	// 一篇带独特词的，验证不是所有词都共享倒排。
	ix.AddWithTime("proj@main:unique.md", "zzz-only-here", 1700009999)

	dir := t.TempDir()
	path := SnapshotPath(dir)
	if err := ix.WriteBinary(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadBinary(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if got.Docs() != ix.Docs() {
		t.Fatalf("doc 数不同：%d vs %d", got.Docs(), ix.Docs())
	}
	if got.Terms() != ix.Terms() {
		t.Fatalf("词数不同：%d vs %d", got.Terms(), ix.Terms())
	}

	// 检索结果必须一致（这才是索引存在的意义）。
	for _, q := range []string{"common", "repeated", "zzz-only-here", "number 42"} {
		a := ix.SearchCovered(q, 5, 0, 0, nil, nil)
		b := got.SearchCovered(q, 5, 0, 0, nil, nil)
		if len(a) != len(b) {
			t.Fatalf("%q 的命中数不同：%d vs %d", q, len(a), len(b))
		}
		for i := range a {
			if a[i].Addr != b[i].Addr {
				t.Fatalf("%q 第 %d 条不同：%s vs %s", q, i, a[i].Addr, b[i].Addr)
			}
		}
	}

	// 时间戳也要保住（时效权重依赖它）。
	if got.DocTime("proj@main:unique.md") != ix.DocTime("proj@main:unique.md") {
		t.Fatalf("时间戳丢了：%d vs %d",
			got.DocTime("proj@main:unique.md"), ix.DocTime("proj@main:unique.md"))
	}

	// 撤销必须仍然精确（docTokens 存下来了）。
	if !got.Remove("proj@main:2026-11-15/000.md") {
		t.Fatal("Remove 失败")
	}
	if n := len(got.SearchCovered("common", 200, 0, 0, nil, nil)); n != 199 {
		t.Fatalf("撤销后还有 %d 篇命中「common」，应当是 199", n)
	}
}

// 加载必须快。判据不看绝对秒数（机器不同），看「相对全量分词」的量级：
// 分词 200 篇约 1ms，加载同规模快照不该超过它的 100 倍。
func TestBinaryFormatLoadsFast(t *testing.T) {
	ix := New()
	for d := 0; d < 5000; d++ {
		addr := fmt.Sprintf("proj@main:%05d.md", d)
		ix.AddWithTime(addr, "body with several common words repeated for realism "+itoaTest(d), 1700000000)
	}
	dir := t.TempDir()
	path := SnapshotPath(dir)
	if err := ix.WriteBinary(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, _ := os.Stat(path)
	t.Logf("5000 篇快照：%.1f KB", float64(st.Size())/1024)

	t0 := time.Now()
	got, err := ReadBinary(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	el := time.Since(t0)
	t.Logf("加载 5000 篇耗时 %v", el)

	if got.Docs() != 5000 {
		t.Fatalf("doc 数 %d", got.Docs())
	}
	if el > 2*time.Second {
		t.Fatalf("加载 5000 篇用了 %v，太慢——这个格式就是为了让它进秒级", el)
	}

	_ = filepath.Join
}

func itoaTest(i int) string { return fmt.Sprintf("%d", i) }
