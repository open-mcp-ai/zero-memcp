//go:build perf

package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 重启后必须自动预热正文缓存，用户不该为「缓存还没热」等几十秒。
//
// 要防的具体故障（3060 hub 实测）：快照恢复出的 Block 只有元数据，
// Body 为空，于是重启后第一次全库 grep 要从磁盘重新解压 38,183 个
// Git 对象——实测 79.8 秒，而第二次只要 232ms。
//
// 这条测试直接盯住「后台协程把缓存填满了」，不依赖任何时间断言：
// 时间会随机器的快慢变化，而「缓存条目数从 0 变成 N」是确定的事实。
func TestBodyCacheWarmsInBackground(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	for i := 0; i < 30; i++ {
		if _, err := callMCPRaw(t, srv, tok, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@fact:warm" + itoa(i) + ".md $B",
				"vars": map[string]string{"B": "预热内容 " + itoa(i)},
			},
		}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatal(err)
	}

	// 模拟「快照恢复」：Block 只有元数据，Body 为空，缓存清空。
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}
	srv.bodies = newBodyCache(64 << 20)
	for _, tr := range testTrees(srv) {
		for i := range tr.Blocks {
			tr.Blocks[i].Body = nil
		}
	}
	if stored, _, _, _ := srv.bodies.stats(); stored != 0 {
		t.Fatalf("清空后缓存应为空，实际 %d", stored)
	}

	// 手动跑一次预热（测试里不等 3 秒的启动延迟）
	srv.warmBodyCacheNow()

	stored, _, _, usedMB := srv.bodies.stats()
	if stored == 0 {
		t.Fatal("后台预热没有填任何条目——重启后第一次 grep 仍会从磁盘重新解压全库")
	}
	t.Logf("预热完成：%d 条，%.2f MB", stored, usedMB)

	// 预热之后 grep 不应再触发读盘：换一个全新实例对照 put 次数。
	before := srv.bodies.putCount()
	if _, _, err := srv.execGrep("预热内容", nil, Caller{User: "zero", Root: true}, false, false); err != nil {
		t.Fatal(err)
	}
	if after := srv.bodies.putCount(); after != before {
		t.Fatalf("预热后 grep 又新增了 %d 条缓存，说明没有命中", after-before)
	}
}
