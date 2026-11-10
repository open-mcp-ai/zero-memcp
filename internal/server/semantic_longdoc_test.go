package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 真实故障形态：一条超过模型上下文长度的文档会让后端整批返回 413，
// 于是同一批里另外十几条也一起被计成失败——它们完全无辜。
//
// 更糟的是下一轮增量重建会重试，那个长文档再次打死另一批邻居，形成
// 「一个长文档长期吃掉十几个向量」的稳态：索引里永远缺那些片，而
// status 只报一个数字，看不出是谁。
//
// 这个测试钉两件事：
//  1. 整批失败后逐条重试，邻居该嵌进去的都嵌进去
//  2. 超长的先截断再试，截断按字符不按字节（不能切断 UTF-8）
func TestSemanticLongDocDoesNotFailItsBatch(t *testing.T) {
	const testDim = 8
	// 用真实的截断门槛当"模型上下文"，否则测不到截断路径
	const contextChars = maxEmbedChars

	var (
		mu        sync.Mutex
		longestIn int // 后端实际收到过的最长输入（字符数）
		calls     atomic.Int64
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls.Add(1)

		// 任何一条超长 → 整批 413，和 Ollama 的行为一致
		for _, s := range req.Input {
			n := len([]rune(s))
			mu.Lock()
			if n > longestIn {
				longestIn = n
			}
			mu.Unlock()
			if n > contextChars {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"message": "the input length exceeds the context length",
						"type":    "api_error",
					},
				})
				return
			}
		}

		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		var data []item
		for i := range req.Input {
			vec := make([]float32, testDim)
			vec[0] = 1
			data = append(data, item{Embedding: vec, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}

	slices := map[string]string{
		// 邻居：正常长度
		"thought/2026-11-10/a.md": "短的一篇",
		"thought/2026-11-10/b.md": strings.Repeat("中", 50),
		// 超过上下文的那一条（"长" 是 3 字节，按字节截断会切坏 UTF-8）
		"thought/2026-11-10/c.md": strings.Repeat("长", contextChars+500), // 超过门槛 500 字
	}
	for p, body := range slices {
		if err := repo.Write("main.record", p, []byte(body), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Commit("main.record", "seed"); err != nil {
		t.Fatal(err)
	}

	s1, err := New(Config{DataDir: dataDir, DefaultBranch: "main.record", Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	s1.cfg.Semantic.VectorDir = filepath.Join(dataDir, "vectors")
	s1.cfg.Semantic.BatchSize = 10 // 三条同批，长文档就在这一批里
	s1.cfg.Semantic.Providers = []config.SemanticProvider{
		{Label: "mock", Kind: "http", Endpoint: srv.URL, Model: "mock", Dim: testDim},
	}
	s1.initSemantic()
	waitForSemanticReady(t, s1)

	s1.semantic.mu.RLock()
	got := s1.semantic.idx.Len()
	s1.semantic.mu.RUnlock()
	s1.Close()

	// 三条都得进索引：长文档被截断后成功，邻居不受牵连。
	if got != 3 {
		t.Fatalf("应有 3 条向量（长文档截断后也应成功），实际 %d", got)
	}

	mu.Lock()
	defer mu.Unlock()
	// 后端应当既收到过超限的原文（第一次尝试），也收到过截断后的合法长度。
	// 只断言「最终不超过上限」是测不出截断的：上限内本来就会成功。
	if longestIn <= contextChars {
		t.Fatalf("后端从未收到超过 %d 字符的输入，说明这片根本没超限，测试没测到截断", contextChars)
	}
	if calls.Load() < 3 {
		t.Fatalf("应发生整批失败后的逐条重试，实际只有 %d 次请求", calls.Load())
	}
}

// 截断按字符而不是按字节：按字节截断会产生非法 UTF-8，
// 而非法 UTF-8 在 JSON 编码里会被替换成 U+FFFD，送到后端就是一段
// 看似正常、实则被悄悄改过的文本。
func TestTruncateRunesKeepsValidUTF8(t *testing.T) {
	s := strings.Repeat("长", 10) // 30 字节 / 10 字符
	if got := truncateRunes(s, 4); got != strings.Repeat("长", 4) {
		t.Fatalf("按字符截断应得到 4 个汉字，实际 %q", got)
	}
	// 短于上限时原样返回
	if got := truncateRunes(s, 100); got != s {
		t.Fatalf("未超限时应原样返回")
	}
}