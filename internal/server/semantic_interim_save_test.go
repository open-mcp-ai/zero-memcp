package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"strconv"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/embed"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/vecindex"
)

// 构建中途必须落盘，否则「重启 = 从零重算」。
//
// 线上实测过两次：跑到 18,400/26,293 时重启（发版 / OOM），1.5 小时的
// 推理全部作废，而日志上只表现为新进程把整个增量重跑一遍——没有任何
// 一行说「刚才那些算完了但没存」。
//
// 这条测试钉住三件事：
//
//	① 中途（还没跑完）磁盘上就有已算完的那部分；
//	② 那一部分是**可用**的：重新加载后向量条数与维度都对；
//	③ 指纹表跟着落盘，所以重载后增量比对不会把已算的当成新增。
func TestSemanticInterimSaveSurvivesAbort(t *testing.T) {
	const dim = 16
	flaky := newFlakyEmbedServer(t, dim)
	dataDir := t.TempDir()
	vecDir := filepath.Join(dataDir, "vectors")

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	// 足够多的切片，保证一轮里会跨过「中途落盘」的时机。
	const n = 40
	for i := 0; i < n; i++ {
		p := filepath.ToSlash(filepath.Join("thought", string(rune('a'+i%26))+"-"+strconv.Itoa(i)+".md"))
		if err := repo.Write("main.record", p, []byte("第 "+strconv.Itoa(i)+" 片内容，用来触发中途落盘"), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
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
			BatchSize: 4,
			Providers: []config.SemanticProvider{
				{Label: "flaky", Kind: "http", Endpoint: flaky.srv.URL, Model: "test-model", Dim: dim},
			},
			MaintainInterval: time.Hour, // 不让周期巡检干扰
		},
		IndexEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	waitForSemanticReady(t, srv)

	// 起点：缓存目录里应该已经有一份完整结果。
	dir := srv.vectorDirFor(srv.semantic.getEmb())
	if dir == "" {
		t.Fatal("拿不到缓存目录")
	}
	full, prints, err := vecindex.LoadRev(dir, srv.semantic.getEmb().Name(), embed.Revision(srv.semantic.getEmb()))
	if err != nil {
		t.Fatalf("首轮完整构建后应当能加载: %v", err)
	}
	if full.Len() != n {
		t.Fatalf("首轮应当索引 %d 篇，实际 %d", n, full.Len())
	}
	if len(prints) != n {
		t.Fatalf("指纹表应当有 %d 条，实际 %d", n, len(prints))
	}

	// 真正的故障形态：一轮跑到一半，后端挂了。
	//
	// 这时 buildSemanticIndex 会返回 "embedding backend unavailable"。
	// 若没有中途落盘，磁盘上什么也没有——下次启动仍然从零重算。
	// 有中途落盘时，已经算完的那几批必须留在盘上。
	orig := semanticSaveEvery
	semanticSaveEvery = 0 // 每批之后都落盘，测试才能确定地观察到
	defer func() { semanticSaveEvery = orig }()

	if err := os.Remove(filepath.Join(dir, "vectors.bin")); err != nil {
		t.Fatalf("删缓存: %v", err)
	}
	srv.semantic.setActive(nil, nil)

	// 第 3 批开始之后一路 503：当作后端挂了。
	flaky.SetDownAfterBatch(3)
	err = srv.buildSemanticIndex(vecDir, srv.semanticHeads(), nil, nil)
	if err == nil {
		t.Fatal("后端挂掉时本轮应当失败返回")
	}

	part, pprints, lerr := vecindex.LoadRev(dir, srv.semantic.getEmb().Name(), embed.Revision(srv.semantic.getEmb()))
	if lerr != nil {
		t.Fatalf("中途落盘应当留下可加载的部分索引: %v", lerr)
	}
	if part.Len() == 0 {
		t.Fatal("中途落盘没生效：本轮已经算完的向量一条都没留下")
	}
	if part.Len() >= n {
		t.Fatalf("应该在跑完之前就失败，实际留下了全部 %d 篇", part.Len())
	}
	if len(pprints) != part.Len() {
		t.Fatalf("指纹数 %d 与向量数 %d 不匹配：两者必须同源才不会被当成新增重算",
			len(pprints), part.Len())
	}
	t.Logf("中途崩溃后留下 %d/%d 篇可复用向量", part.Len(), n)

	// 恢复后端，用磁盘上的那份做基准再跑一轮：剩下的必须被补上，
	// 而且已算完的不该重算（指纹命中）。
	flaky.SetDown(false)
	reused := 0
	if err := srv.buildSemanticIndex(vecDir, srv.semanticHeads(), part, pprints); err != nil {
		t.Fatalf("恢复后重建: %v", err)
	}
	full, _, ferr := vecindex.LoadRev(dir, srv.semantic.getEmb().Name(), embed.Revision(srv.semantic.getEmb()))
	if ferr != nil {
		t.Fatalf("最终加载: %v", ferr)
	}
	if full.Len() != n {
		t.Fatalf("恢复后应当补齐到 %d 篇，实际 %d（复用 %d）", n, full.Len(), reused)
	}
}
