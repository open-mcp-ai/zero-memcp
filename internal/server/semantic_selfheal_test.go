package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// flakyEmbedServer 模拟 Embedding 服务：可以随时切成故障/恢复。
type flakyEmbedServer struct {
	srv      *httptest.Server
	down     atomic.Bool
	calls    atomic.Int64
	mu       sync.Mutex
	seenText []string
	dim      int
}

func newFlakyEmbedServer(t *testing.T, dim int) *flakyEmbedServer {
	t.Helper()
	f := &flakyEmbedServer{dim: dim}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 故障态：模拟 Ollama 没起来 / 3060 断网
		if f.down.Load() {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.calls.Add(int64(len(req.Input)))
		f.mu.Lock()
		f.seenText = append(f.seenText, req.Input...)
		f.mu.Unlock()

		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		out := make([]item, 0, len(req.Input))
		for i := range req.Input {
			v := make([]float32, f.dim)
			v[0] = 1
			out = append(out, item{Embedding: v, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *flakyEmbedServer) SetDown(v bool) { f.down.Store(v) }

// TestSemanticSelfHealsAfterOutage 锁定自愈闭环：
//
//	① 启动时嵌入服务可用 → 建好初始索引
//	② 服务挂掉 → 写入新记忆（只能走 BM25）
//	③ 服务恢复 → 后台巡检自动把新记忆补进向量索引
//
// 这是用户问的那个问题：「嵌入失败后恢复，能自动差量重建吗」。
// 答案是：能，靠指纹比对 + 周期巡检，不需要重启。
func TestSemanticSelfHealsAfterOutage(t *testing.T) {
	const dim = 16
	flaky := newFlakyEmbedServer(t, dim)

	dataDir := t.TempDir()
	vecDir := filepath.Join(dataDir, "vectors")

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	// ① 先有两篇
	for _, p := range []string{"thought/a.md", "thought/b.md"} {
		if err := repo.Write("main.record", p, []byte("content "+p), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Commit("main.record", "seed"); err != nil {
		t.Fatal(err)
	}

	newSrv := func() *Server {
		s, err := New(Config{
			DataDir:       dataDir,
			DefaultBranch: "main.record",
			Listen:        "127.0.0.1:0",
			Semantic: struct {
				LibPath          string
				ModelPath        string
				VocabPath        string
				VectorDir        string
				Providers        []config.SemanticProvider
				BatchSize        int
				MaintainInterval time.Duration
			}{
				VectorDir: vecDir,
				BatchSize: 10,
				Providers: []config.SemanticProvider{
					{Label: "flaky", Kind: "http", Endpoint: flaky.srv.URL, Model: "test-model", Dim: dim},
				},
				// 测试里把巡检周期压到 150ms
				MaintainInterval: 150 * time.Millisecond,
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return s
	}

	srv := newSrv()
	defer srv.Close()
	waitForSemanticReady(t, srv)

	if n := srv.semantic.idx.Len(); n != 2 {
		t.Fatalf("初始应索引 2 篇，实际 %d", n)
	}

	// ② 服务挂掉，通过服务写入新记忆（走真实的 addBlockLocked 路径）
	flaky.SetDown(true)
	flaky.calls.Store(0)
	// 用 put 写入，服务自己会更新内存树快照 + 调用 kickSemantic
	_, perr := srv.execTerm("put daily@main.record/thought/c.md -- written during outage", Viewer{}, rootCaller())
	if perr != nil {
		t.Fatalf("put during outage: %v", perr)
	}

	// 等一两个巡检周期：索引不该变多（服务不可用）
	time.Sleep(500 * time.Millisecond)
	if n := srv.semantic.idx.Len(); n != 2 {
		t.Fatalf("服务不可用期间索引不应增长，实际 %d", n)
	}

	// ③ 服务恢复，等巡检自动补算
	flaky.SetDown(false)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		srv.semantic.mu.RLock()
		ix := srv.semantic.idx
		ready := srv.semantic.ready
		srv.semantic.mu.RUnlock()
		if ready && ix != nil && ix.Len() >= 3 {
			// 恢复成功
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	srv.semantic.mu.RLock()
	defer srv.semantic.mu.RUnlock()
	t.Fatalf("服务恢复后 6 秒内未自动补算：索引 %d 篇，err=%q",
		func() int {
			if srv.semantic.idx == nil {
				return 0
			}
			return srv.semantic.idx.Len()
		}(), srv.semantic.err)
}

// TestSemanticStartsDownThenRecovers 锁定更苛刻的场景：
//
//	启动时嵌入服务就不可用 → 语义索引完全是空的
//	→ 服务后来起来 → 后台巡检自动建立整个索引
//
// 旧行为下这是死局：initSemantic 失败后永远不会再尝试，
// 必须重启服务。而「先启动 zero-memcp，后启动 Ollama」是常态。
func TestSemanticStartsDownThenRecovers(t *testing.T) {
	const dim = 16
	flaky := newFlakyEmbedServer(t, dim)
	flaky.SetDown(true) // 启动时就不可用

	dataDir := t.TempDir()
	vecDir := filepath.Join(dataDir, "vectors")

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main.record", "thought/x.md", []byte("hello"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main.record", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:       dataDir,
		DefaultBranch: "main.record",
		Listen:        "127.0.0.1:0",
		Semantic: struct {
			LibPath          string
			ModelPath        string
			VocabPath        string
			VectorDir        string
			Providers        []config.SemanticProvider
			BatchSize        int
			MaintainInterval time.Duration
		}{
			VectorDir: vecDir,
			BatchSize: 10,
			Providers: []config.SemanticProvider{
				{Label: "flaky", Kind: "http", Endpoint: flaky.srv.URL, Model: "test-model", Dim: dim},
			},
			MaintainInterval: 150 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	// 启动后应处于「不可用」状态，且不 panic
	srv.semantic.mu.RLock()
	embNil := srv.semantic.emb == nil
	srv.semantic.mu.RUnlock()
	if !embNil {
		t.Skip("启动时意外连上了 mock 服务，此用例不适用")
	}

	// 恢复服务，等巡检自动建索引
	flaky.SetDown(false)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		srv.semantic.mu.RLock()
		ready := srv.semantic.ready
		var n int
		if srv.semantic.idx != nil {
			n = srv.semantic.idx.Len()
		}
		srv.semantic.mu.RUnlock()
		if ready && n >= 1 {
			return // 自愈成功
		}
		time.Sleep(50 * time.Millisecond)
	}
	srv.semantic.mu.RLock()
	errMsg := srv.semantic.err
	srv.semantic.mu.RUnlock()
	t.Fatalf("启动时不可用、后来恢复，但索引始终没建起来（err=%q）", errMsg)
}
