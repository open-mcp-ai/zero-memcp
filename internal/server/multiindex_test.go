package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/vecindex"
)

// 假嵌入器：把文本映射成确定性的定长向量，便于验证「每套索引
// 用的是自己的模型」而不是偷用别人的。
type fakeEmbedder struct {
	name string
	dim  int
	// calls 记录被调用次数，用来证明两路都真的查了。
	calls *int
}

func (f *fakeEmbedder) Dim() int     { return f.dim }
func (f *fakeEmbedder) Name() string { return f.name }
func (f *fakeEmbedder) Close() error { return nil }

func (f *fakeEmbedder) Embed(text string) ([]float32, error) {
	*f.calls++
	return f.vec(text), nil
}

func (f *fakeEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.vec(t)
	}
	return out, nil
}

func (f *fakeEmbedder) vec(text string) []float32 {
	v := make([]float32, f.dim)
	for i, r := range []rune(text) {
		v[i%f.dim] += float32(r%7) + 1
	}
	// 归一化，符合「余弦相似度」的前提
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1)
	_ = inv
	l := float32(0)
	for _, x := range v {
		l += x * x
	}
	return l2norm(v)
}

func l2norm(v []float32) []float32 {
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return v
	}
	// 简单的开方近似：用两次牛顿迭代
	r := sum
	for i := 0; i < 24; i++ {
		r = 0.5 * (r + sum/r)
	}
	for i := range v {
		v[i] /= r
	}
	return v
}

// 两套索引必须能同时活着，各自用**自己的**模型嵌入，查询时两路都查，
// 最后按名次（RRF）融合——绝不比较两个模型的原始分数。
func TestMultiIndexFanOutAndRRF(t *testing.T) {
	srv, _ := setupMountServer(t)

	// 两套不同维度、不同模型的索引（维度不同正好证明空间互不可比）。
	zhCalls, enCalls := 0, 0
	zhEmb := &fakeEmbedder{name: "zh-model", dim: 8, calls: &zhCalls}
	enEmb := &fakeEmbedder{name: "en-model", dim: 16, calls: &enCalls}

	zhIdx := vecindex.New(8)
	enIdx := vecindex.New(16)

	// 中文索引：命中 "zh-hit"
	if err := zhIdx.Add("proj@zero:thought/2026-11-19/zh-hit.md", zhEmb.vec("中文记忆片段")); err != nil {
		t.Fatal(err)
	}
	// 英文索引：命中 "en-hit"
	if err := enIdx.Add("proj@zero:thought/2026-11-19/en-hit.md", enEmb.vec("english memory slice")); err != nil {
		t.Fatal(err)
	}

	zh := &liveIndex{key: "zh-model-aaaa", model: "zh-model", lang: "zh", emb: zhEmb}
	zh.store(zhIdx)
	en := &liveIndex{key: "en-model-bbbb", model: "en-model", lang: "en", emb: enEmb}
	en.store(enIdx)

	srv.liveIdx = &liveIndexes{}
	srv.liveIdx.put(zh)
	srv.liveIdx.put(en)

	if got := srv.liveIdx.count(); got != 2 {
		t.Fatalf("应当同时持有 2 套索引，实际 %d", got)
	}

	hits := srv.semanticSearchAll("中文记忆片段 english memory slice")
	if len(hits) == 0 {
		t.Fatal("多索引扇出没有任何命中")
	}
	// 两路的嵌入器都被调用过：证明每套索引用自己模型重算查询向量，
	// 而不是拿一套模型的坐标去查另一套的空间。
	if zhCalls == 0 || enCalls == 0 {
		t.Fatalf("两路都应被查询，实际 zh=%d en=%d", zhCalls, enCalls)
	}

	// 两套索引各自的头部命中都应当出现在融合结果里。
	var hasZh, hasEn bool
	for _, h := range hits {
		if h.ID == "proj@zero:thought/2026-11-19/zh-hit.md" {
			hasZh = true
		}
		if h.ID == "proj@zero:thought/2026-11-19/en-hit.md" {
			hasEn = true
		}
	}
	if !hasZh || !hasEn {
		t.Fatalf("融合结果应同时含两套索引的命中：zh=%v en=%v hits=%v", hasZh, hasEn, hits)
	}

	// 融合分必须降序（RRF 的输出是有序的）
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Fatalf("融合结果未按分数降序：第 %d 个 %f < 第 %d 个 %f", i-1, hits[i-1].Score, i, hits[i].Score)
		}
	}

	// 诊断标签要能说出两套索引各自多少条
	if label := srv.liveIndexLabel(); label == "" {
		t.Fatal("liveIndexLabel 应报告在用的索引")
	} else {
		t.Logf("在用索引: %s", label)
	}
}

// RRF 的判据：同一文档被两路都命中时，它的融合分必须高于只被单路命中的。
func TestMergeRRFRewardsAgreement(t *testing.T) {
	// 路 1：a 第一，shared 第二
	listA := []vecindex.Hit{{ID: "a"}, {ID: "shared"}}
	// 路 2：b 第一，shared 第三
	listB := []vecindex.Hit{{ID: "b"}, {ID: "x"}, {ID: "shared"}}

	merged := mergeRRF([][]vecindex.Hit{listA, listB})
	if len(merged) != 4 {
		t.Fatalf("去重后应有 4 条，实际 %d", len(merged))
	}
	if merged[0].ID != "shared" {
		t.Fatalf("两路都命中的 shared 应排第一，实际 %s", merged[0].ID)
	}
}
