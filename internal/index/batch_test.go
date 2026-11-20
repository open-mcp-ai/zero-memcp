package index

import (
	"fmt"
	"testing"
)

// AddBatch 与串行 AddWithTime 的索引结果必须完全等价。
//
// 判据：文档数相等、词数相等、同一条查询检索到的篇目及得分完全一致。
func TestAddBatchEquivalence(t *testing.T) {
	docs := []BatchDoc{
		{Addr: "a", Text: "这是第一篇测试记忆，包含中文和 english keywords 1729", ModTime: 100},
		{Addr: "b", Text: "第二篇完全不同的内容，关于 zero-memcp 的全局无锁设计", ModTime: 200},
		{Addr: "c", Text: "第三篇也有 1729，并且提到了中文二元分词", ModTime: 300},
		{Addr: "d", Text: "", ModTime: 400}, // 空文档边界
	}

	// 串行版
	serial := New()
	for _, d := range docs {
		serial.AddWithTime(d.Addr, d.Text, d.ModTime)
	}

	// 批量并行版（开 4 个 worker）
	batch := New()
	batch.AddBatch(docs, 4)

	if serial.Docs() != batch.Docs() {
		t.Fatalf("文档数不一致：串行 %d vs 批量 %d", serial.Docs(), batch.Docs())
	}
	if serial.Terms() != batch.Terms() {
		t.Fatalf("词数不一致：串行 %d vs 批量 %d", serial.Terms(), batch.Terms())
	}

	// 查询对拍
	queries := []string{"中文", "1729", "无锁", "keywords", "不存在的词"}
	for _, q := range queries {
		h1 := serial.SearchCovered(q, 10, 0.1, 0.0, nil, nil)
		h2 := batch.SearchCovered(q, 10, 0.1, 0.0, nil, nil)
		if len(h1) != len(h2) {
			t.Fatalf("查询 %q 命中数不一致：串行 %d vs 批量 %d", q, len(h1), len(h2))
		}
		for i := range h1 {
			if h1[i].Addr != h2[i].Addr {
				t.Fatalf("查询 %q 第 %d 名地址不一致：%s vs %s", q, i, h1[i].Addr, h2[i].Addr)
			}
			// 得分应完全一致（浮点误差内）
			if diff := h1[i].Score - h2[i].Score; diff > 1e-6 || diff < -1e-6 {
				t.Fatalf("查询 %q 第 %d 名得分不一致：%f vs %f", q, i, h1[i].Score, h2[i].Score)
			}
		}
	}
}

// 并行分词性能对比：量真实加速比。
func TestAddBatchPerf(t *testing.T) {
	const N = 2000
	docs := make([]BatchDoc, N)
	for i := 0; i < N; i++ {
		docs[i] = BatchDoc{
			Addr:    fmt.Sprintf("fact/%05d.md", i),
			Text:    fmt.Sprintf("正文段落 %d，中英混排 english tokens 1729，验证并行分词吞吐", i),
			ModTime: int64(i),
		}
	}

	// 串行
	s := New()
	for _, d := range docs {
		s.AddWithTime(d.Addr, d.Text, d.ModTime)
	}

	// 批量
	b := New()
	b.AddBatch(docs, 8)

	if s.Docs() != b.Docs() {
		t.Fatalf("docs mismatch: %d vs %d", s.Docs(), b.Docs())
	}
}

// 大语料 + 高度重叠的词表：这才是 addMany 的合并路径。
//
// 小样本对拍会漏掉真实故障：5000 篇共享同一批常见中文二元组时，
// 倒排表的合并/覆盖分支才会被走到。
func TestAddBatchEquivalenceLargeCorpus(t *testing.T) {
	const N = 500
	docs := make([]BatchDoc, N)
	for i := 0; i < N; i++ {
		// 共享词：中文段落/english keywords/1729；独有词：编号
		docs[i] = BatchDoc{
			Addr:    fmt.Sprintf("fact/%05d.md", i),
			Text:    fmt.Sprintf("共享的中文段落 english keywords 1729，独有编号 %d，再加一点 content 让它更长一些", i),
			ModTime: int64(1000 + i),
		}
	}

	serial := New()
	for _, d := range docs {
		serial.AddWithTime(d.Addr, d.Text, d.ModTime)
	}
	batch := New()
	batch.AddBatch(docs, 8)

	if serial.Docs() != batch.Docs() {
		t.Fatalf("文档数不一致：%d vs %d", serial.Docs(), batch.Docs())
	}
	if serial.Terms() != batch.Terms() {
		t.Fatalf("词数不一致：%d vs %d", serial.Terms(), batch.Terms())
	}

	for _, q := range []string{"共享", "english", "1729", "独有编号 42", "content"} {
		h1 := serial.SearchCovered(q, 20, 0.1, 0.0, nil, nil)
		h2 := batch.SearchCovered(q, 20, 0.1, 0.0, nil, nil)
		if len(h1) != len(h2) {
			t.Fatalf("查询 %q 命中数不一致：%d vs %d", q, len(h1), len(h2))
		}
		for i := range h1 {
			if h1[i].Addr != h2[i].Addr {
				t.Fatalf("查询 %q 第 %d 名不一致：%s vs %s", q, i, h1[i].Addr, h2[i].Addr)
			}
			if d := h1[i].Score - h2[i].Score; d > 1e-6 || d < -1e-6 {
				t.Fatalf("查询 %q 第 %d 名得分不一致：%f vs %f", q, i, h1[i].Score, h2[i].Score)
			}
		}
	}
}

// 同一地址重复写入：批量路径的「后写覆盖」必须与串行一致。
func TestAddBatchOverwriteSameAddr(t *testing.T) {
	first := []BatchDoc{{Addr: "a", Text: "第一版内容 alpha", ModTime: 1}}
	second := []BatchDoc{{Addr: "a", Text: "第二版内容 beta", ModTime: 2}}

	ix := New()
	ix.AddBatch(first, 2)
	ix.AddBatch(second, 2)

	// 旧词不应残留（这正是 AddWithTime 里 Remove 的作用）
	if got := ix.SearchCovered("alpha", 10, 0.1, 0.0, nil, nil); len(got) != 0 {
		t.Fatalf("覆盖后仍能搜到旧词 alpha：%v", got)
	}
	if got := ix.SearchCovered("beta", 10, 0.1, 0.0, nil, nil); len(got) == 0 {
		t.Fatal("覆盖后搜不到新词 beta")
	}
	if ix.Docs() != 1 {
		t.Fatalf("覆盖后应只有 1 篇，实际 %d", ix.Docs())
	}
}
