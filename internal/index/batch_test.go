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

// RemoveBatch 与逐条 Remove 必须完全等价。
//
// 判据不能只看「文档数变成 0」——真正的风险是残留：
// 某个词的倒排里还留着已撤 addr、或 totalDocs/sumDocLen 记错。
// 所以这里用「撤一半」来对拍：剩下的那一半必须和从没加过被撤文档的
// 索引查到一模一样的结果。
func TestRemoveBatchEquivalence(t *testing.T) {
	docs := []BatchDoc{
		{Addr: "keep/1", Text: "要留下的第一篇，关于中文分词与倒排索引", ModTime: 100},
		{Addr: "keep/2", Text: "要留下的第二篇，也提到 zero-memcp 与 1729", ModTime: 200},
		{Addr: "gone/1", Text: "要撤掉的第一篇，独有词只有它才有 zzzunique", ModTime: 300},
		{Addr: "gone/2", Text: "要撤掉的第二篇，共享词中文分词 1729 也在里面", ModTime: 400},
		{Addr: "gone/3", Text: "", ModTime: 500}, // 空文档边界：只有时间记录
	}
	gone := []string{"gone/1", "gone/2", "gone/3"}

	// 逐条撤
	serial := New()
	serial.AddBatch(docs, 2)
	for _, a := range gone {
		serial.Remove(a)
	}

	// 批量撤
	batch := New()
	batch.AddBatch(docs, 2)
	batch.RemoveBatch(gone)

	// 从没加过被撤文档的索引（真相）
	truth := New()
	truth.AddBatch(docs[:2], 2)

	if serial.Docs() != batch.Docs() || batch.Docs() != truth.Docs() {
		t.Fatalf("文档数不一致：逐条 %d vs 批量 %d vs 真相 %d", serial.Docs(), batch.Docs(), truth.Docs())
	}
	if serial.Terms() != batch.Terms() || batch.Terms() != truth.Terms() {
		t.Fatalf("词数不一致：逐条 %d vs 批量 %d vs 真相 %d", serial.Terms(), batch.Terms(), truth.Terms())
	}

	queries := []string{"中文", "分词", "1729", "zzzunique", "zero-memcp", "要撤掉的"}
	for _, q := range queries {
		h1 := serial.SearchCovered(q, 10, 0.1, 0.0, nil, nil)
		h2 := batch.SearchCovered(q, 10, 0.1, 0.0, nil, nil)
		h3 := truth.SearchCovered(q, 10, 0.1, 0.0, nil, nil)
		if len(h1) != len(h2) || len(h2) != len(h3) {
			t.Fatalf("查询 %q 命中数不一致：逐条 %d vs 批量 %d vs 真相 %d", q, len(h1), len(h2), len(h3))
		}
		for i := range h1 {
			if h1[i].Addr != h2[i].Addr || h2[i].Addr != h3[i].Addr {
				t.Fatalf("查询 %q 第 %d 名不一致：%s / %s / %s", q, i, h1[i].Addr, h2[i].Addr, h3[i].Addr)
			}
			if d := h1[i].Score - h2[i].Score; d > 1e-6 || d < -1e-6 {
				t.Fatalf("查询 %q 第 %d 名得分不一致：%f vs %f", q, i, h1[i].Score, h2[i].Score)
			}
		}
	}

	// 共享词必须还留着一半：撤过头的症状是「剩下的也搜不到了」。
	if got := batch.SearchCovered("1729", 10, 0.1, 0.0, nil, nil); len(got) != 1 || got[0].Addr != "keep/2" {
		t.Fatalf("共享词 1729 该只剩 keep/2，实际 %v", got)
	}

	// 幂等：再撤一次不该出错、不该把别人的记录也带走。
	batch.RemoveBatch(gone)
	if batch.Docs() != truth.Docs() || batch.Terms() != truth.Terms() {
		t.Fatalf("重复撤改变了索引状态：docs %d vs %d, terms %d vs %d",
			batch.Docs(), truth.Docs(), batch.Terms(), truth.Terms())
	}
}
