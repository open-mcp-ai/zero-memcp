package index_test

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/index"
)

// TestWeightBoost 验证：
// 1. 两个词频相同的条目，被读过的排在前面。
// 2. 即使被读过 100 次，完全不相关的词也不会被拉上来（上限 1.6x 保护）。
func TestWeightBoost(t *testing.T) {
	ix := index.New()
	ix.Add("doc-fresh", "termcp 端口配置说明")
	ix.Add("doc-popular", "termcp 端口配置指南")
	ix.Add("doc-irrelevant", "完全无关的做饭菜谱")

	weights := map[string]int{
		"doc-popular":    20,
		"doc-irrelevant": 1000,
	}
	wf := func(addr string) int { return weights[addr] }

	// 查端口：popular 被读过 20 次，得分必须高于 fresh
	hits := ix.SearchWeighted("termcp 端口", 10, 0, wf)
	if len(hits) < 2 {
		t.Fatalf("期望命中至少 2 个，得到 %d", len(hits))
	}
	if hits[0].Addr != "doc-popular" {
		t.Errorf("期望 doc-popular 排第一，实际是 %s (scores: popular=%.3f, fresh=%.3f)",
			hits[0].Addr, hits[0].Score, hits[1].Score)
	}

	// 保护验证：查做饭，doc-popular 绝不能因为权重高就混进结果
	hitsCooking := ix.SearchWeighted("做饭", 10, 0, wf)
	for _, h := range hitsCooking {
		if h.Addr == "doc-popular" {
			t.Errorf("doc-popular 词不匹配，绝不该因为权重高就被查出来")
		}
	}
}
