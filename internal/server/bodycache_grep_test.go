//go:build perf

package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// grep 必须走正文缓存：稳态下不得每次从 Git 对象里重新解压全库。
//
// 要防的具体故障：readBlock 原本只有 Block.Body 这一层「扫描期临时
// 缓存」，索引建完就清空（见 blockbody_cache_test.go）。于是稳态下
// 每次 grep 都把全库正文 zlib 解压一遍——实测 2,000 块 594ms，
// 按线性外推到 38,000 篇约 11 秒，而 MCP 调用有超时，症状就是
// 「grep 老是超时」。
//
// 判据不是「变快了」这种模糊说法，而是「第二次读同一块不再触发
// Read()」。用计数器直接盯住这一点，机器再慢也不会误判。
func TestGrepUsesBodyCache(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "c", Cap: "rwx"})

	for i := 0; i < 20; i++ {
		if _, err := callMCPRaw(t, srv, tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@fact:k" + itoa(i) + ".md $B",
				"vars": map[string]string{"B": "缓存标记 " + itoa(i)},
			},
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}

	// 索引阶段会把正文收进缓存，所以这里统计的是「之后又读了几次盘」。
	stored0, _, _, _ := srv.bodies.stats()
	if stored0 == 0 {
		t.Fatal("索引完成后缓存仍为空——说明 readBlock 走了 Body 分支却没写缓存，" +
			"重启后第一次 grep 仍会把全库重新解压一遍")
	}

	before := srv.bodies.putCount() // 插入次数，用于确认第二次查询没有再新增
	hits0 := srv.bodies.hitCount()

	caller := Caller{User: "zero", Root: true}
	if _, _, err := srv.execGrep("缓存标记", nil, caller, false, false); err != nil {
		t.Fatal(err)
	}
	afterFirst := srv.bodies.putCount()
	if afterFirst != before {
		t.Fatalf("第一次 grep 竟然新增了 %d 条缓存——索引阶段本应已经装好", afterFirst-before)
	}

	// 第二次查询：必须命中缓存
	if _, _, err := srv.execGrep("缓存标记", nil, caller, false, false); err != nil {
		t.Fatal(err)
	}
	hits1 := srv.bodies.hitCount()
	if hits1 <= hits0 {
		t.Fatalf("第二次 grep 应当命中缓存，命中数没有增长（%d → %d）", hits0, hits1)
	}
	stored, hits, skipped, usedMB := srv.bodies.stats()
	t.Logf("缓存统计: stored=%d hits=%d skipped=%d used=%.1fMB", stored, hits, skipped, usedMB)
}

// 关掉缓存后 grep 结果必须完全一致：缓存只影响快慢，不影响正确性。
func TestGrepResultIdenticalWithoutCache(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "d", Cap: "rwx"})

	for i := 0; i < 10; i++ {
		if _, err := callMCPRaw(t, srv, tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@fact:m" + itoa(i) + ".md $B",
				"vars": map[string]string{"B": "一致性子 " + itoa(i)},
			},
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}

	caller := Caller{User: "zero", Root: true}
	withCache, _, err := srv.execGrep("一致性子", nil, caller, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// 换成禁用缓存的实例重跑
	srv.bodies = newBodyCache(0)
	noCache, _, err := srv.execGrep("一致性子", nil, caller, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if withCache != noCache {
		t.Fatalf("开/关缓存的 grep 结果不一致：\n--- with cache ---\n%s\n--- no cache ---\n%s", withCache, noCache)
	}
}
