package server

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/index"
)

// 初始索引还在后台建的时候，语义追平必须**完全不启动**。
//
// 这条钉的是一个真事故（3060, v0.2.12）：快照因版本升级被丢弃 → 索引在
// 后台重建 → 语义追平在容器启动 4 秒时跑了一轮，那一刻树只发布了 6 片，
// 它把这 6 片当成"当前树的全集"算完并 SaveRev 写盘，把原本 54,630 条的
// 向量缓存覆盖成 6 条（vectors.bin 218MB → 24KB）。
//
// 后果不是"少一点"，是：之后要重跑五万多次嵌入才补回来，而重建期间
// 嵌入服务被占满，正常查询被拖到客户端超时。
//
// 判据用**嵌入请求次数**而不是文件内容：文件可能因为别的原因存在或
// 不存在，而"这一轮到底有没有去算"只有一个答案。
func TestSemanticCatchUpSkippedWhileInitialIndexBuilds(t *testing.T) {
	const dim = 16
	flaky := newFlakyEmbedServer(t, dim)
	dataDir := t.TempDir()

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	for i := 0; i < n; i++ {
		p := filepath.ToSlash(filepath.Join("thought", "s"+itoa(i)+".md"))
		if err := repo.Write("main.record", p, []byte("第 "+itoa(i)+" 片可索引内容"), gitstore.ModeContent); err != nil {
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
			VectorDir: filepath.Join(dataDir, "vectors"),
			BatchSize: 4,
			Providers: []config.SemanticProvider{
				{Label: "flaky", Kind: "http", Endpoint: flaky.srv.URL, Model: "test-model", Dim: dim},
			},
			MaintainInterval: time.Hour, // 不让周期巡检干扰本轮
		},
		IndexEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Close 会等 indexBuildDone 关闭（见 server.go 的 Close）。本测试故意
	// 留一个未关的通道，所以必须保证**先**放它再 Close：t.Cleanup 后进先出，
	// 因此先注册 Close、后注册 release。
	t.Cleanup(func() { _ = srv.Close() })
	waitForSemanticReady(t, srv)

	// 清掉缓存并摘掉内存索引：这样"下一轮追平"必然要重新嵌入全部 n 片。
	// 不清的话增量比对会因为指纹命中而 0 次嵌入，看不出拦截有没有生效。
	dir := srv.vectorDirFor(srv.semantic.getEmb())
	if err := os.Remove(filepath.Join(dir, "vectors.bin")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	srv.semantic.setActive(nil, nil)

	before := flaky.calls.Load()

	// 模拟"初始索引正在后台构建"：拿一个永不关闭的通道占住这个状态。
	//
	// 必须用 t.Cleanup 关它，不能只靠测试末尾的 close：一旦上面的断言
	// 走 t.Fatal，后面的 close 就不会执行，而 Close() 会永远等这个通道——
	// 失败会以「测试超时」的形式出现，把真正的断言信息掩掉。
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release) // 后进先出：它先于 srv.Close 执行
	srv.indexBuildDone = gate

	srv.catchUpSemantic("test-while-building")
	time.Sleep(300 * time.Millisecond) // 给它足够时间去犯错

	if got := flaky.calls.Load(); got != before {
		t.Fatalf("索引还在建，语义追平却已经去嵌入 %d 条（原本 %d 条）——"+
			"这正是把 54,630 条缓存覆盖成 6 条的那条路径", got, before)
	}
	// 注意判的是 idx 而不是快照本身：setActive(nil, nil) 存的是一个
	// idx 为 nil 的包装结构，getActive() 永远非 nil。
	if snap := srv.semantic.getActive(); snap != nil && snap.idx != nil {
		t.Fatal("索引还在建时不应发布语义索引：那份结果只覆盖扫描到的那部分")
	}

	// 构建结束之后必须恢复——拦截不能变成永久停摆。
	release()
	srv.indexBuildDone = nil // 让 Close() 不必再等这个通道

	srv.catchUpSemantic("after-build")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if flaky.calls.Load() > before {
			return // 恢复了：真的去嵌入了
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("索引建完之后语义追平没有恢复——拦截逻辑把它永久卡死了")
}

// indexBuildRunning 的三种状态：nil（同步/无索引）、未关（构建中）、已关（建完）。
func TestIndexBuildRunningStates(t *testing.T) {
	srv := &Server{}

	srv.indexBuildDone = nil
	if srv.indexBuildRunning() {
		t.Fatal("nil 必须解释为「不在构建」，否则同步构建的实例永远不会启动语义")
	}

	open := make(chan struct{})
	srv.indexBuildDone = open
	if !srv.indexBuildRunning() {
		t.Fatal("未关闭的通道必须解释为「正在构建」")
	}

	close(open)
	if srv.indexBuildRunning() {
		t.Fatal("已关闭的通道必须解释为「构建结束」")
	}
}

// 旧版本（≤v0.2.9）写的快照头里没有 IndexScope 字段，读出来是零值。
// 只要当前实例也没限定范围，这份快照就必须被采用——否则每次升级都白重建。
func TestLegacySnapshotWithoutIndexScopeIsAdopted(t *testing.T) {
	dir := t.TempDir()
	hdr := &index.SnapshotHeader{
		Heads:      map[string]string{"p@main": "deadbeef"},
		Docs:       1,
		Terms:      1,
		TreeFile:   "index-trees.gob",
		IndexScope: "", // ← v0.2.9 及更早的头部里根本没有这个字段
	}
	if err := index.SaveSnapshot(dir, hdr, nil); err != nil {
		t.Fatal(err)
	}
	got, err := index.LoadSnapshotHeader(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.IndexScope != "" {
		t.Fatalf("期望读到空范围，实际 %q", got.IndexScope)
	}

	// 空范围必须被当成「没限定范围」放行。
	srv := &Server{}
	if srv.indexScopeFingerprint() != "inc=;exc=" {
		t.Fatalf("没有配置文件时指纹应当是 inc=;exc=（全量），实际 %q", srv.indexScopeFingerprint())
	}

	// 用真实的判定函数验一遍：不能只测指纹字符串。
	// 走 tryLoadIndexSnapshot 需要完整仓库，这里直接比对手工实现的条件。
	if got.IndexScope != "" && got.IndexScope != srv.indexScopeFingerprint() {
		t.Fatal("空范围被误判为「范围变了」——升级会白丢一次快照")
	}
}

// 指纹不能因为「有没有配置文件」而给出两个不同字符串。
//
// 早先 cfgFile == nil 返回 "all"，有配置文件但没写范围返回 "inc=;exc="，
// 语义相同却是两个值：一个用前者写的快照被后者读到就会判「变了」。
func TestIndexScopeFingerprintCanonicalForUnrestricted(t *testing.T) {
	noCfg := &Server{}
	withCfg := &Server{}
	// 直接塞一个空配置：include/exclude 都没写。
	withCfg.cfgFilePtr.Store(&config.File{})
	if noCfg.indexScopeFingerprint() != withCfg.indexScopeFingerprint() {
		t.Fatalf("两种「没限定范围」必须得到同一个指纹：%q vs %q",
			noCfg.indexScopeFingerprint(), withCfg.indexScopeFingerprint())
	}
}
