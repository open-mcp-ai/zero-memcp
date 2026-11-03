package embed

import (
	"os"
	"path/filepath"
	"testing"
)

func findAssets(t *testing.T) Config {
	t.Helper()
	// 从测试目录往上找 assets / models
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		LibPath:   filepath.Join(root, "assets", "libonnxruntime.1.31.1.dylib"),
		ModelPath: filepath.Join(root, "models", "bge-small-zh-int8.onnx"),
		VocabPath: filepath.Join(root, "models", "vocab.txt"),
	}
	if _, err := os.Stat(cfg.LibPath); err != nil {
		t.Skipf("dylib not present: %v", err)
	}
	if _, err := os.Stat(cfg.ModelPath); err != nil {
		t.Skipf("model not present: %v", err)
	}
	return cfg
}

func TestEmbedBasic(t *testing.T) {
	cfg := findAssets(t)
	emb, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer emb.Close()

	v, err := emb.Embed("今天天气很好")
	if err != nil {
		t.Fatal(err)
	}
	// 维度不写死：不同模型输出不同（bge 512 / LaBSE 768），
	// 以模型实测为准，再交叉验证 Embedder 自报的维度与向量长度一致。
	if len(v) != emb.Dim() {
		t.Fatalf("vector len %d != embedder dim %d", len(v), emb.Dim())
	}
	if emb.Dim() <= 0 {
		t.Fatalf("embedder dim not detected: %d", emb.Dim())
	}

	// 模长应为 1
	var norm float32
	for _, x := range v {
		norm += x * x
	}
	if norm < 0.99 || norm > 1.01 {
		t.Fatalf("norm expected ~1.0, got %f", norm)
	}
}

func TestEmbed1729CrossLingual(t *testing.T) {
	cfg := findAssets(t)
	emb, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer emb.Close()

	// 真实案发现场：中文提问 vs 英文思考
	q, _ := emb.Embed("1729这个数字有什么含义")
	target, _ := emb.Embed("Port 1729 — the Hardy–Ramanujan number, beautiful fit for a memory service")
	irrel, _ := emb.Embed("量子色动力学描述强相互作用的物理理论")

	simTarget := Dot(q, target)
	simIrrel := Dot(q, irrel)

	t.Logf("sim(q, target) = %.4f", simTarget)
	t.Logf("sim(q, irrel)  = %.4f", simIrrel)

	if simTarget <= simIrrel {
		t.Fatalf("target (%.4f) should score higher than irrelevant (%.4f)", simTarget, simIrrel)
	}
	if simTarget-simIrrel < 0.05 {
		t.Fatalf("margin (%.4f) too small", simTarget-simIrrel)
	}
}
