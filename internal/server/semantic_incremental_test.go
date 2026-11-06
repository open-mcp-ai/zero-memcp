package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// TestSemanticIncremental 锁定增量索引的闭环行为：
//
//	首轮全量构建（N 次推理）
//	  ↓ 二次启动（内容无改动）
//	0 次推理，100% 复用缓存
//	  ↓ 本地新增 1 篇
//	仅 1 次推理，其余 N 篇全复用
//	  ↓ 状态落盘
//	manifest 里的指纹与向量一一对应
func TestSemanticIncremental(t *testing.T) {
	var embedCalls atomic.Int64
	const testDim = 32

	// 模拟一个统计调用次数的 HTTP embedder 服务
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		embedCalls.Add(int64(len(req.Input)))

		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		var data []item
		for i := range req.Input {
			vec := make([]float32, testDim)
			vec[0] = 1.0 // 归一化伪向量
			data = append(data, item{Embedding: vec, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	vecDir := filepath.Join(dataDir, "vectors")

	// 准备一个仓库，写 3 篇 thought
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"thought/2026-11-06/1.md",
		"thought/2026-11-06/2.md",
		"thought/2026-11-06/3.md",
	} {
		if err := repo.Write("main.record", p, []byte("content of "+p), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Commit("main.record", "seed"); err != nil {
		t.Fatal(err)
	}

	// ── 1. 首轮全量构建 ──
	s1, err := New(Config{
		DataDir:       dataDir,
		DefaultBranch: "main.record",
		Listen:        "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 手工注入 HTTP provider 配置与向量目录
	s1.cfg.Semantic.VectorDir = vecDir
	s1.cfg.Semantic.BatchSize = 10
	mockProviders := []config.SemanticProvider{
		{Label: "mock", Kind: "http", Endpoint: srv.URL, Model: "mock-model", Dim: testDim},
	}
	s1.cfg.Semantic.Providers = mockProviders
	s1.initSemantic()

	// 等后台构建完成
	waitForSemanticReady(t, s1)
	if calls := embedCalls.Load(); calls != 4 { // 1 probe + 3 块
		t.Fatalf("首轮推理次数应为 4（1 probe + 3 块），实际 %d", calls)
	}
	s1.Close()

	// ── 2. 二次启动（内容无改动）──
	// 期望：0 次推理，3 篇全从旧索引复用
	embedCalls.Store(0)
	s2, err := New(Config{
		DataDir:       dataDir,
		DefaultBranch: "main.record",
		Listen:        "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	s2.cfg.Semantic.VectorDir = vecDir
	s2.cfg.Semantic.BatchSize = 10
	s2.cfg.Semantic.Providers = mockProviders
	s2.initSemantic()

	waitForSemanticReady(t, s2)
	// 仅 1 次 probe，3 篇内容无改动，不应有新的 embedding 产生
	if calls := embedCalls.Load(); calls != 1 {
		t.Fatalf("无改动二次启动推理次数应为 1（仅 probe，0 块），实际 %d", calls)
	}
	if s2.semantic.idx.Len() != 3 {
		t.Fatalf("复用后向量总数应为 3，实际 %d", s2.semantic.idx.Len())
	}
	s2.Close()

	// ── 3. 新增 1 篇 thought，并修改其中 1 篇 ──
	// 期望：只有新增的 1 篇 + 改动的 1 篇参与推理（共 2 篇），另 2 篇复用
	embedCalls.Store(0)
	if err := repo.Write("main.record", "thought/2026-11-06/4.md", []byte("brand new content"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main.record", "thought/2026-11-06/1.md", []byte("EDITED content"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main.record", "edit & add"); err != nil {
		t.Fatal(err)
	}

	s3, err := New(Config{
		DataDir:       dataDir,
		DefaultBranch: "main.record",
		Listen:        "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	s3.cfg.Semantic.VectorDir = vecDir
	s3.cfg.Semantic.BatchSize = 10
	s3.cfg.Semantic.Providers = mockProviders
	s3.initSemantic()

	waitForSemanticReady(t, s3)
	// 1 probe + 2 块（修改的 1.md + 新增的 4.md）= 3
	if calls := embedCalls.Load(); calls != 3 {
		t.Fatalf("部分改动推理次数应为 3（1 probe + 2 块），实际 %d", calls)
	}
	if s3.semantic.idx.Len() != 4 {
		t.Fatalf("增量后向量总数应为 4，实际 %d", s3.semantic.idx.Len())
	}
	s3.Close()
}

func waitForSemanticReady(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.semantic.mu.RLock()
		ready := s.semantic.ready
		building := s.semantic.building
		s.semantic.mu.RUnlock()
		if ready && !building {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待语义索引就绪超时")
}
