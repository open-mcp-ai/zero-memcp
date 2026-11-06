package embed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// mockEmbeddingServer 模拟一个 OpenAI 兼容的 /v1/embeddings 接口。
func mockEmbeddingServer(t *testing.T, dim int, failRate int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		if failRate > 0 && int(n)%failRate == 0 {
			http.Error(w, "simulated 503", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		var data []item
		for i := range req.Input {
			vec := make([]float32, dim)
			// 用确定的伪随机填满，让结果可比
			for d := 0; d < dim; d++ {
				vec[d] = float32(i+1) / float32(d+1)
			}
			data = append(data, item{Embedding: vec, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	return srv, &calls
}

// TestHTTPEmbedderBasic 验证 HTTP provider 的 probe、单条和批量。
func TestHTTPEmbedderBasic(t *testing.T) {
	srv, calls := mockEmbeddingServer(t, 1024, 0)
	defer srv.Close()

	emb, err := OpenHTTP(HTTPConfig{
		Endpoint: srv.URL,
		Model:    "BAAI/bge-m3",
		Dim:      1024,
	})
	if err != nil {
		t.Fatalf("OpenHTTP: %v", err)
	}
	defer emb.Close()

	if emb.Dim() != 1024 {
		t.Fatalf("Dim = %d, want 1024", emb.Dim())
	}
	if emb.Name() != "BAAI/bge-m3" {
		t.Fatalf("Name = %s", emb.Name())
	}

	// 批量
	vs, err := emb.EmbedBatch([]string{"first", "second", "third"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vs) != 3 {
		t.Fatalf("len = %d, want 3", len(vs))
	}
	for i, v := range vs {
		if len(v) != 1024 {
			t.Errorf("v[%d] len = %d, want 1024", i, len(v))
		}
	}
	// probe 1 次 + 批量 1 次 = 2 次调用
	if c := calls.Load(); c != 2 {
		t.Errorf("服务端调用次数 = %d, want 2", c)
	}
}

// TestChainDegrade 验证降级链：链首失败时自动降级到第二顺位。
func TestChainDegrade(t *testing.T) {
	// primary 坏掉（一请求就 503）
	badSrv, _ := mockEmbeddingServer(t, 768, 1)
	defer badSrv.Close()

	// fallback 正常
	goodSrv, goodCalls := mockEmbeddingServer(t, 768, 0)
	defer goodSrv.Close()

	chain, err := OpenChain(ChainConfig{
		Providers: []ProviderConfig{
			{Label: "3060", Kind: "http", Endpoint: badSrv.URL, Model: "test", Dim: 768, Timeout: 100 * time.Millisecond},
			{Label: "local", Kind: "http", Endpoint: goodSrv.URL, Model: "test", Dim: 768},
		},
	})
	if err != nil {
		t.Fatalf("OpenChain: %v", err)
	}
	defer chain.Close()

	// 链上两个都注册了，但 badSrv 在 probe 时就会失败
	// OpenChain 应该跳过它，只保留 goodSrv
	if len(chain.providers) != 1 {
		t.Fatalf("应该只保留可用的 1 个后端，得到 %d", len(chain.providers))
	}
	if chain.ActiveLabel() != "local" {
		t.Fatalf("当前生效应为 local，得到 %s", chain.ActiveLabel())
	}

	// 调用正常
	v, err := chain.Embed("hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != 768 {
		t.Fatalf("len = %d", len(v))
	}
	_ = goodCalls
}

// TestChainModelMismatchRejected 链上两个后端如果维度或模型不同，必须拒绝。
//
// 这是防止「静默检索分叉」的核心防线：3060 是 1024 维的 bge-m3，
// 本地是 768 维的 LaBSE，混在一条链上就是灾难。
func TestChainModelMismatchRejected(t *testing.T) {
	s1, _ := mockEmbeddingServer(t, 1024, 0)
	defer s1.Close()
	s2, _ := mockEmbeddingServer(t, 768, 0)
	defer s2.Close()

	_, err := OpenChain(ChainConfig{
		Providers: []ProviderConfig{
			{Label: "p1", Kind: "http", Endpoint: s1.URL, Model: "bge-m3", Dim: 1024},
			{Label: "p2", Kind: "http", Endpoint: s2.URL, Model: "labse", Dim: 768},
		},
	})
	if err == nil {
		t.Fatal("不同维度的后端混在一条链上应该报错，却成功了")
	}
}
