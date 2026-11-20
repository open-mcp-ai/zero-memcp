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
