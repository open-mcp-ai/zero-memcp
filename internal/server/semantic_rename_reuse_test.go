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

// TestSemanticReuseAcrossRenamedPath 锁定「改路径不重算」这条底线。
//
// 场景：文件换到别的路径（规整分桶、挪目录、换分支前缀都会这样），
// 内容一个字没改。
//
// 期望：新地址的向量直接向旧地址借，零次嵌入调用。
//
// 为什么必须锁住：向量只取决于内容，跟路径无关。若按地址判定复用，
// 3 万篇规整就会触发 3 万次嵌入调用——线上代价是几十分钟的 Ollama
// 排队，而其中没有一个字是新的。
func TestSemanticReuseAcrossRenamedPath(t *testing.T) {
	var embedCalls atomic.Int64
	const testDim = 32

	// 计数用的模拟嵌入服务：返回同一个伪向量即可，只关心调用了几次。
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			vec[0] = 1.0
			data = append(data, item{Embedding: vec, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer embSrv.Close()

	dataDir := t.TempDir()
	vecDir := filepath.Join(dataDir, "vectors")
	repoPath := filepath.Join(dataDir, "repos", "daily.git")

	repo, err := gitstore.Init(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	const origPath = "thought/2026-11-06/1.md"
	for _, p := range []string{
		origPath,
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

	providers := []config.SemanticProvider{
		{Label: "mock", Kind: "http", Endpoint: embSrv.URL, Model: "mock-model", Dim: testDim},
	}
	start := func() *Server {
		s, nerr := New(Config{
			DataDir:       dataDir,
			DefaultBranch: "main.record",
			Listen:        "127.0.0.1:0",
		})
		if nerr != nil {
			t.Fatal(nerr)
		}
		s.cfg.Semantic.VectorDir = vecDir
		s.cfg.Semantic.BatchSize = 10
		s.cfg.Semantic.MaintainInterval = 50 * time.Millisecond
		s.cfg.Semantic.Providers = providers
		s.initSemantic()
		return s
	}

	// waitVecs 等到向量库里出现指定地址，或数量达到 n。
	//
	// 不能只依赖 semantic.isReady()：缓存在磁盘上时 ready 会立刻置位，
	// 而增量补算是后台异步的。提前退出会让 t.TempDir() 先在主 goroutine
	// 里清理目录，后台构建于是读到「对象一个个消失」的假象——
	// 那是测试自身的问题，不是存储的问题。
	waitVecs := func(s *Server, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if snap := s.semantic.getActive(); snap != nil && snap.idx != nil && snap.idx.Len() >= want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("等待 %d 条向量超时", want)
	}

	// ── 1. 首轮：3 篇全量嵌入（+1 次探针）──
	s1 := start()
	waitVecs(s1, 3)
	if calls := embedCalls.Load(); calls != 4 {
		t.Fatalf("首轮应为 4 次（1 probe + 3 块），实际 %d", calls)
	}
	s1.Close()

	// ── 2. 只改路径，内容一个字不动 ──
	// 重新打开仓库：模拟「另一个客户端发起规整」，与服务器实例无关。
	repo, err = gitstore.Init(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	const newPath = "thought/2026-11-06/048/1.md"
	if err := repo.MoveAs("main.record", origPath, "main.record", newPath); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := repo.Commit("main.record", "rebucket"); err != nil {
		t.Fatal(err)
	}

	// ── 3. 重启：不该有任何嵌入调用（探针除外）──
	embedCalls.Store(0)
	s2 := start()
	waitVecs(s2, 3)
	time.Sleep(200 * time.Millisecond) // 给可能的重算留出暴露窗口

	if calls := embedCalls.Load(); calls != 1 {
		t.Fatalf("改名后应 0 次重算（仅 1 次探针），实际 %d —— 说明仍在按地址判定复用", calls)
	}
	snap := s2.semantic.getActive()
	if snap == nil || snap.idx.Len() != 3 {
		t.Fatalf("改名后向量数应仍为 3")
	}
	newID := "daily@main.record:" + newPath
	if !snap.idx.Has(newID) {
		t.Fatalf("改名后的新地址没有向量：%s", newID)
	}
	s2.Close()
}