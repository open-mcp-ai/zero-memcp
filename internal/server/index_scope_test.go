package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 按分支控制索引 + 索引是动态过程。
//
// 三条要求逐条验：
//  1. 能按分支控制哪些建索引、哪些不建；
//  2. 启动即可用（不必等索引建完）；
//  3. 索引中可见进度（done/total），且「未命中」必须改口——绝不让调用方
//     把「还没索引到」读成「记忆里没有」。
func TestIndexScopeAndAsyncProgress(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[index]
enabled = true
exclude = ["BlackEggAI/zero@big"]
async = true

[[user]]
name = "zero"
root = true

[[project]]
name  = "BlackEggAI/zero"
owner = "zero"
mode  = "rwxr-xr-x"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", filepath.ToSlash(dataDir))
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "BlackEggAI", "zero.git"))
	if err != nil {
		t.Fatal(err)
	}
	// 默认分支放可检索内容；big 分支放「只有它才有」的独特词。
	if err := repo.Write("zero", "small.md", []byte("可检索的词 SMALLONLY\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fork("zero", "big"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("big", "huge.md", []byte("只在被排除分支里的词 BIGONLY\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("big", "seed big"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 等后台索引跑完（异步构建），再验范围。
	deadline := func() bool {
		p := srv.indexProgressNow()
		return p.Phase == "ready" || p.Phase == "failed"
	}
	for i := 0; i < 600 && !deadline(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if p := srv.indexProgressNow(); p.Phase != "ready" {
		t.Fatalf("后台索引未就绪: %+v", p)
	}

	// ① 范围：被排除的分支不进索引（recall 搜不到），但 read/ls 照常。
	if out, e := rawTerm(t, srv, tok, "recall SMALLONLY", nil); e != "" || !strings.Contains(out, "small.md") {
		t.Fatalf("参与索引的分支应能 recall 到: err=%q out=%q", e, out)
	}
	recallBig, _ := rawTerm(t, srv, tok, "recall BIGONLY", nil)
	if strings.Contains(recallBig, "huge.md") {
		t.Fatalf("被排除的分支不该出现在 recall 结果里:\n%s", recallBig)
	}
	// 但内容依然可读（排除的是索引，不是数据）。
	if out, e := rawTerm(t, srv, tok, "read BlackEggAI/zero@big:huge.md", nil); e != "" || !strings.Contains(out, "BIGONLY") {
		t.Fatalf("被排除分支的内容必须仍可 read: err=%q out=%q", e, out)
	}
	// ls 也必须还能列出它（读的是树，不是索引）。
	if out, e := rawTerm(t, srv, tok, "ls --scope BlackEggAI/zero@big", nil); e != "" || !strings.Contains(out, "huge.md") {
		t.Fatalf("被排除分支应仍能被 ls 列出: err=%q out=%q", e, out)
	}

	// ② 护栏：显式 --scope 指名被排除分支时必须报错，绝不回「没有命中」。
	_, e := rawTerm(t, srv, tok, "recall BIGONLY --scope BlackEggAI/zero@big", nil)
	if e == "" {
		t.Fatal("显式点名被排除的分支必须报错，不能静默回「没有命中」")
	}
	if !strings.Contains(e, "excluded from indexing") {
		t.Fatalf("报错要说清是被索引范围排除的，实际: %s", e)
	}

	// ③ status 必须报出范围与进度（可见性）。
	st, _ := rawTerm(t, srv, tok, "status", nil)
	if !strings.Contains(st, "index: ready") {
		t.Fatalf("status 应报索引就绪，实际:\n%s", st)
	}
	if !strings.Contains(st, "NOT indexed") {
		t.Fatalf("status 必须说明有分支未进索引，实际:\n%s", st)
	}

	// ④ config index 改范围立即生效（不重启）。
	if out, e := rawTerm(t, srv, tok, `config index --exclude "BlackEggAI/zero@big"`, nil); e != "" {
		t.Fatalf("config index 失败: %s", e)
	} else if !strings.Contains(out, "applied") {
		t.Fatalf("config index 应报告已生效: %q", out)
	}
	// 再改回「全建」：big 分支的独特词应能 recall 到。
	if _, e := rawTerm(t, srv, tok, "config index --reset", nil); e != "" {
		t.Fatalf("config index --reset 失败: %s", e)
	}
	for i := 0; i < 600 && srv.indexProgressNow().Building(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	after, _ := rawTerm(t, srv, tok, "recall BIGONLY", nil)
	if !strings.Contains(after, "huge.md") {
		t.Fatalf("重置范围后该分支应重新进入索引:\n%s", after)
	}
}

// 索引构建中：状态可见、已建好的部分能搜到、未命中必须改口。
func TestIndexBuildingIsVisibleAndUsable(t *testing.T) {
	// 直接验进度对象的三件事：Line 有 done/total、Percent、Building。
	p := indexProgress{Phase: "building", Done: 12000, Total: 28000}
	line := p.Line()
	for _, want := range []string{"12000/28000", "42%", "cover only what is built"} {
		if !strings.Contains(line, want) {
			t.Fatalf("进度行应含 %q，实际: %q", want, line)
		}
	}
	if p.Percent() != 42 || !p.Building() {
		t.Fatalf("Percent/Building 不对: %d %v", p.Percent(), p.Building())
	}
	// Total 未知时不能报 0%——那会被读成「一点都没建」。
	unk := indexProgress{Phase: "scanning"}
	if unk.Percent() != -1 {
		t.Fatalf("Total 未知时应返回 -1，实际 %d", unk.Percent())
	}
	if !strings.Contains(unk.Line(), "scanning") {
		t.Fatalf("扫描阶段的描述不对: %q", unk.Line())
	}
	// ready / failed 两态也要说清楚。
	ready := indexProgress{Phase: "ready", Done: 28000, Total: 28000}
	if ready.Building() || !strings.Contains(ready.Line(), "ready") {
		t.Fatalf("ready 态不对: %q", ready.Line())
	}
	failed := indexProgress{Phase: "failed", Done: 5, Total: 10, Err: "boom"}
	if !strings.Contains(failed.Line(), "FAILED") || !strings.Contains(failed.Line(), "boom") {
		t.Fatalf("失败态必须明说原因: %q", failed.Line())
	}
}

// 配置范围指纹：改了 [index] 就必须弃用旧快照，否则刚排除的分支会从
// 快照里原样回来——「配置没生效但看起来生效了」。
func TestIndexScopeFingerprintInvalidatesSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	writeCfg := func(exclude string) string {
		body := "[server]\ndata = \"" + filepath.ToSlash(dataDir) + "\"\n\n[index]\nenabled = true\n" + exclude +
			"\n\n[[user]]\nname = \"zero\"\nroot = true\n"
		path := filepath.Join(dataDir, "zero-memcp.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	s1, err := New(Config{DataDir: dataDir, ConfigFile: writeCfg(`exclude = ["a@b"]`), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	fp1 := s1.indexScopeFingerprint()
	_ = s1.Close()

	s2, err := New(Config{DataDir: dataDir, ConfigFile: writeCfg(`exclude = ["a@c"]`), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	fp2 := s2.indexScopeFingerprint()
	_ = s2.Close()

	if fp1 == fp2 {
		t.Fatalf("范围不同必须产生不同指纹: %q", fp1)
	}
	if !strings.Contains(fp1, "a@b") {
		t.Fatalf("指纹应反映实际范围: %q", fp1)
	}
}

// 在真正的 Server 上模拟「正在建索引（Phase=building，索引里只有一半）」：
//  1. 已经写进 idx 的那一半立刻能 recall 到，且尾部带构建提示；
//  2. 还没建进去的那一半，recall 必须改口说「已建好部分没命中」，绝不说 no matches。
func TestIndexMidBuildRecallBehavesHonesty(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 测试默认未在配置里启 index 时补打开以便直接测 mid-build recall。
	srv.indexEnabled = true

	// 往索引里放一半数据，并把进度置为「正在建 500/2000」。
	if _, e := rawTerm(t, srv, tok, "write blackegg/me@zero/half.md $B",
		map[string]string{"B": "已建好的一半 HALFBUILT"}); e != "" {
		t.Fatal(e)
	}
	srv.setIndexProgress(indexProgress{
		Phase: "building", Done: 500, Total: 2000, StartedAt: time.Now(),
	})

	// ① 已建好的那一半能搜到，且明确标着「结果只覆盖已建部分」。
	hit, e := rawTerm(t, srv, tok, "recall HALFBUILT", nil)
	if e != "" {
		t.Fatalf("构建中 recall 应可用: %s", e)
	}
	if !strings.Contains(hit, "half.md") || !strings.Contains(hit, "500/2000") || !strings.Contains(hit, "25%") {
		t.Fatalf("构建中命中必须既返回内容又标明进度，实际:\n%s", hit)
	}

	// ② 还没索引到的词：绝不能静默返回裸 `no matches`（会被当成库里没有）。
	miss, _ := rawTerm(t, srv, tok, "recall NOTINDEXEDYET", nil)
	if !strings.Contains(miss, "indexed portion") || !strings.Contains(miss, "500/2000") {
		t.Fatalf("构建中未命中必须明说「在已建部分里未命中 + 进度」，实际: %q", miss)
	}

	// ③ status 同样必须反映 building 进度。
	st, _ := rawTerm(t, srv, tok, "status", nil)
	if !strings.Contains(st, "building 500/2000") || !strings.Contains(st, "25%") {
		t.Fatalf("status 必须展示进度，实际:\n%s", st)
	}
}

// 被排除的分支不进正文缓存：否则预热会把那几万条大分支的正文全解压进内存
// （实测 201 MB / 6 GB 常驻里的大头），「排除」就只排掉了倒排表。
func TestExcludedBranchIsNotWarmedIntoBodyCache(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[index]
enabled = true
exclude = ["BlackEggAI/zero@big"]

[search]
body_cache_mb = 64

[[user]]
name = "zero"
root = true

[[project]]
name  = "BlackEggAI/zero"
owner = "zero"
mode  = "rwxr-xr-x"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", filepath.ToSlash(dataDir))
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "BlackEggAI", "zero.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "kept.md", []byte("保留的内容 KEPT\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fork("zero", "big"); err != nil {
		t.Fatal(err)
	}
	// 被排除分支放 6 篇，保留分支只有 1 篇：数量差异让断言能分辨。
	for i := 0; i < 6; i++ {
		if err := repo.Write("big", fmt.Sprintf("2020-01-01/big-%d.md", i),
			[]byte(fmt.Sprintf("大分支内容 BIG-%d\n", i)), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Commit("big", "seed big"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir: dataDir, ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"), Listen: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	for i := 0; i < 400 && srv.indexProgressNow().Building(); i++ {
		time.Sleep(25 * time.Millisecond)
	}

	srv.warmBodyCacheNow()
	stored, _, _, _ := srv.bodies.stats()
	// 保留分支 1 篇（+ SKILL.md 之类若存在）；被排除的 6 篇一篇都不该进来。
	if stored > 2 {
		t.Fatalf("被排除的分支不该被预热进正文缓存：stored=%d（保留分支只有 1 篇）", stored)
	}
	if stored == 0 {
		t.Fatal("保留分支的内容应被预热（否则这条断言分不清「跳过」和「没跑」）")
	}
}
