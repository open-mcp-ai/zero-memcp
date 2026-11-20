package server

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// seedDedup 绝不能把全库历史切片全读一遍。
//
// 判重表的唯一用途是「防同一轮写入重复投递」（网络重试），
// 这种重复永远发生在当下。读 30 天前的历史切片防今天的重复
// 毫无意义——实测 25,000 块全读要 47 秒，占了快照启动耗时的 95%。
//
// 这条测试钉住：只读最近 72 小时的切片，历史切片一次读盘都不许碰。
func TestSeedDedupDoesNotReadHistoricalSlices(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 写一条「今天」的和一条「两周前」的
	today := time.Now().Format("2006-01-02")
	oldDate := "2026-01-01"

	if _, err := callMCPVars(t, srv, tok,
		fmt.Sprintf("blackegg/me@fact:%s/now.md", today),
		"---\nkind: thought\nsession: s1\n---\n\ncurrent content"); err != nil {
		t.Fatalf("write today: %v", err)
	}
	if _, err := callMCPVars(t, srv, tok,
		fmt.Sprintf("blackegg/me@fact:%s/old.md", oldDate),
		"---\nkind: thought\nsession: s1\n---\n\nancient content"); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 挂读盘计数器
	var oldReads atomic.Int64
	var todayReads atomic.Int64
	mutateTrees(t, srv, func(trees []*memory.Tree) {
		for _, tr := range trees {
			for i := range tr.Blocks {
				b := tr.Blocks[i]
				orig := b.Read
				if orig == nil {
					continue
				}
				path := b.Path
				tr.Blocks[i].Read = func() ([]byte, error) {
					if dateInPath(path) == oldDate {
						oldReads.Add(1)
					}
					if dateInPath(path) == today {
						todayReads.Add(1)
					}
					return orig()
				}
			}
		}
	})

	srv.seedDedup(testTrees(srv))

	if n := oldReads.Load(); n > 0 {
		t.Fatalf("seedDedup 读了 %d 次历史切片——它应当只看最近 72 小时", n)
	}
	if n := todayReads.Load(); n == 0 {
		t.Fatal("seedDedup 连今天的切片都没读——判重表没预热上")
	}
}

// dateInPath 单测
func TestDateInPath(t *testing.T) {
	cases := map[string]string{
		"2026-11-15/01.md":            "2026-11-15",
		"thought/2026-11-15/01.md":    "2026-11-15",
		"deep/tree/2026-01-02/foo.md": "2026-01-02",
		"SKILL.md":                    "",
		"not-a-date/foo.md":           "",
		"2026-99-99/x.md":             "2026-99-99", // 结构匹配即可
	}
	for in, want := range cases {
		if got := dateInPath(in); got != want {
			t.Errorf("dateInPath(%q) = %q, want %q", in, got, want)
		}
	}
}
