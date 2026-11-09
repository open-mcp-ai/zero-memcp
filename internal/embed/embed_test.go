package embed

import "testing"

// Dot 是纯 float32 运算，与推理后端无关，所以它必须在
// CGO_ENABLED=0 下也能测——这正是它从 embed.go 搬出来的原因。

func TestDotBasic(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if got := Dot(a, b); got != 1 {
		t.Fatalf("同向量内积应为 1，得到 %v", got)
	}
	c := []float32{0, 1, 0}
	if got := Dot(a, c); got != 0 {
		t.Fatalf("正交向量内积应为 0，得到 %v", got)
	}
}

func TestDotLengthMismatchUsesShorter(t *testing.T) {
	// 短的一侧决定长度：这是调用方依赖的既有行为，不是巧合。
	if got := Dot([]float32{1, 1}, []float32{1}); got != 1 {
		t.Fatalf("应按较短的向量截断，得到 %v", got)
	}
}
