package embed

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTestStaticModel 现场写一个合法的 .zmstatic 模型供测试用。
func buildTestStaticModel(t *testing.T, dim int, words map[string][]float32) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("ZMST")
	_ = binary.Write(&b, binary.LittleEndian, uint32(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(dim))
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(words)))

	keys := make([]string, 0, len(words))
	for k := range words {
		keys = append(keys, k)
	}
	for _, k := range keys {
		_ = binary.Write(&b, binary.LittleEndian, uint16(len(k)))
		b.WriteString(k)
	}
	for _, k := range keys {
		vec := words[k]
		for _, x := range vec {
			_ = binary.Write(&b, binary.LittleEndian, math.Float32bits(x))
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "test.zmstatic")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStaticEmbedRoundTrip(t *testing.T) {
	if !staticCompiled {
		t.Skip("此二进制未编译 -tags static，跳过静态嵌入测试")
	}
	const dim = 4
	p := buildTestStaticModel(t, dim, map[string][]float32{
		"apple":  {1, 0, 0, 0},
		"banana": {0, 1, 0, 0},
		"苹果":     {0.707, 0.707, 0, 0},
	})
	emb, err := OpenStatic(StaticConfig{ModelPath: p, Name: "test-static"})
	if err != nil {
		t.Fatalf("OpenStatic: %v", err)
	}
	if emb.Dim() != dim {
		t.Fatalf("dim want %d, got %d", dim, emb.Dim())
	}
	v, err := emb.Embed("apple banana")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != dim {
		t.Fatalf("vector len want %d, got %d", dim, len(v))
	}
	// 归一化后的向量长度必须是 1（允许微小浮点误差）
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if math.Abs(sum-1.0) > 1e-4 {
		t.Fatalf("L2 模长不是 1: %f", sum)
	}
}

func TestStaticUncompiledReportsHelpfulError(t *testing.T) {
	if staticCompiled {
		t.Skip("此二进制已编译 -tags static，测试未编译时的错误信息不适用")
	}
	_, err := OpenStatic(StaticConfig{ModelPath: "any.zmstatic"})
	if err == nil {
		t.Fatal("未编译时 OpenStatic 应报错，实际返回 nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tags static") {
		t.Fatalf("错误信息必须教用户如何重新编译，实际为：%q", msg)
	}
}
