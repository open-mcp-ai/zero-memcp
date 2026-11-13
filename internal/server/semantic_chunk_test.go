package server

import (
	"math"
	"testing"
	"time"
)

// 长文档必须被切块嵌入后取语义中心，而不是截断丢尾巴。
func TestSplitRunesCoversWholeDoc(t *testing.T) {
	doc := make([]rune, 0, 5000)
	for i := 0; i < 5000; i++ {
		doc = append(doc, rune('a'+i%26))
	}
	chunks := splitRunes(doc, embedChunkChars)
	total := 0
	for _, c := range chunks {
		if n := len([]rune(c)); n > embedChunkChars {
			t.Fatalf("chunk longer than window: %d", n)
		}
		total += len([]rune(c))
	}
	if total != len(doc) {
		t.Errorf("chunks lost content: %d of %d runes", total, len(doc))
	}
}

// 均值向量必须是单位长：下游按余弦打分，模长不一会让长文档被系统性
// 压低或抬高，排序变成「按文档长度排」。
func TestMeanVectorIsNormalised(t *testing.T) {
	// 嵌入模型产出的向量本来就是单位向量；两个同样重要性的块模长相同。
	v := meanVector([][]float32{{1, 0, 0}, {0, 1, 0}, nil})
	if v == nil {
		t.Fatal("nil mean")
	}
	var sum float64
	for _, x := range v {
		sum += float64(x * x)
	}
	if math.Abs(math.Sqrt(sum)-1) > 1e-5 {
		t.Errorf("mean vector not unit length: |v| = %f", math.Sqrt(sum))
	}
	if math.Abs(float64(v[0])-float64(v[1])) > 1e-6 {
		t.Errorf("mean should weigh both chunks equally, got %v", v)
	}
}

// 失败退避：连续失败时间隔必须增长，且封顶 2 小时。
func TestSemanticFailureBackoffGrows(t *testing.T) {
	s := &Server{}
	var prev time.Duration
	for i := 0; i < 6; i++ {
		s.noteSemanticFailure("x.md", "boom")
		e := s.semantic.quarantine["x.md"]
		wait := time.Until(e.nextTry)
		// 到达封顶后各次等待相同，留 1 秒容差避免把「封顶」判成「退步」。
		if i > 0 && wait < prev-1*time.Second {
			t.Errorf("backoff shrank at attempt %d: %v < %v", i+1, wait, prev)
		}
		if wait > 2*time.Hour+time.Minute {
			t.Errorf("backoff exceeded cap: %v", wait)
		}
		prev = wait
	}
	if s.semantic.quarantine["x.md"].failures != 6 {
		t.Errorf("failure counter not tracked")
	}
}
