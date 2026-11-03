package vecindex

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// testDim 与实现常量一致；测试里单列出来，改维度时只需改一处。
const testDim = Dim

func unitVec(seed int64) []float32 {
	r := rand.New(rand.NewSource(seed))
	v := make([]float32, testDim)
	var norm float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		norm += float64(v[i]) * float64(v[i])
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func TestAddAndSearch(t *testing.T) {
	ix := New(testDim)
	// 造 100 条随机单位向量
	for i := 0; i < 100; i++ {
		if err := ix.Add(string(rune('A'+i%26))+string(rune('a'+i/26)), unitVec(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if ix.Len() != 100 {
		t.Fatalf("expected 100, got %d", ix.Len())
	}

	// 用第 42 条自己的向量查询，自己必须排第一且分数≈1
	q := unitVec(42)
	hits := ix.Search(q, 5, 0)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Score < 0.99 {
		t.Fatalf("self-match score too low: %f", hits[0].Score)
	}
}

func TestSearchThreshold(t *testing.T) {
	ix := New(testDim)
	ix.Add("a", unitVec(1))
	ix.Add("b", unitVec(2))

	// 门限 0.99 应该只留自匹配
	hits := ix.Search(unitVec(1), 10, 0.99)
	if len(hits) != 1 || hits[0].ID != "a" {
		t.Fatalf("expected only 'a', got %+v", hits)
	}
}

func TestUpdateInPlace(t *testing.T) {
	ix := New(testDim)
	v1 := unitVec(10)
	ix.Add("x", v1)
	v2 := unitVec(20)
	ix.Add("x", v2) // 同一 ID 更新
	if ix.Len() != 1 {
		t.Fatalf("update should not grow, got %d", ix.Len())
	}
	hits := ix.Search(v2, 1, 0)
	if hits[0].Score < 0.99 {
		t.Fatalf("updated vector not used: %f", hits[0].Score)
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	ix := New(testDim)
	ix.Add("p@b/x.md", unitVec(7))
	ix.Add("p@b/y.md", unitVec(8))

	heads := map[string]string{"b": "abc123"}
	if err := ix.Save(dir, "test-model", heads); err != nil {
		t.Fatal(err)
	}

	// 相同身份应能载回
	ix2, err := Load(dir, "test-model", heads)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ix2.Len() != 2 {
		t.Fatalf("expected 2, got %d", ix2.Len())
	}
	hits := ix2.Search(unitVec(7), 1, 0)
	if hits[0].ID != "p@b/x.md" {
		t.Fatalf("wrong id after reload: %s", hits[0].ID)
	}

	// 模型变了 → 必须判过期
	if _, err := Load(dir, "other-model", heads); err != ErrStale {
		t.Fatalf("expected ErrStale on model change, got %v", err)
	}

	// head 变了 → 必须判过期
	if _, err := Load(dir, "test-model", map[string]string{"b": "def456"}); err != ErrStale {
		t.Fatalf("expected ErrStale on head change, got %v", err)
	}
}

func TestLoadMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	if _, err := Load(dir, "m", nil); err != ErrStale {
		t.Fatalf("expected ErrStale, got %v", err)
	}
}

func TestCorruptBinRejected(t *testing.T) {
	dir := t.TempDir()
	ix := New(testDim)
	ix.Add("a", unitVec(1))
	if err := ix.Save(dir, "m", nil); err != nil {
		t.Fatal(err)
	}
	// 截断 bin 文件，模拟写到一半断电
	p := filepath.Join(dir, "vectors.bin")
	if err := os.Truncate(p, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "m", nil); err != ErrStale {
		t.Fatalf("expected ErrStale on truncated bin, got %v", err)
	}
}

func BenchmarkSearch10k(b *testing.B) {
	ix := New(testDim)
	for i := 0; i < 10000; i++ {
		ix.Add(string(rune('a'+i%26))+string(rune('a'+i/26))+"x", unitVec(int64(i)))
	}
	q := unitVec(5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.Search(q, 8, 0.3)
	}
}
