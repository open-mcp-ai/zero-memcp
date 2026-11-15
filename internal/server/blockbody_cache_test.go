package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// Block.Body 是「扫描期缓存」，不是长期状态：索引建完必须扔掉。
//
// 它存在的理由是省掉一次重复读盘（扫描判 frontmatter 已读过 .md，
// 索引阶段原本又读一遍）。但留着它的代价是整库正文常驻内存——
// 24,000 块 × 平均几 KB，而真正需要正文时按需 Read() 就够了。
//
// 这条测试钉的是「不能泄漏成常驻」这个性质。它不会因为功能正确
// 而自然成立——加缓存很容易，忘记清也很容易，而忘记清没有任何症状。
func TestBlockBodyCacheIsReleased(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 铺一些 .md，让扫描期缓存有机会被填上。
	for i := 0; i < 5; i++ {
		if _, err := callMCP(t, srv, tok,
			"put blackegg/me@fact/md"+itoa(i)+" -- body-"+itoa(i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 走一次全量重建，然后检查所有 Block 的 Body 都空了。
	if err := srv.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	srv.mu.RLock()
	var leaked int
	var leakedPath string
	for _, tr := range srv.trees {
		for _, b := range tr.Blocks {
			if b.Body != nil {
				leaked++
				if leakedPath == "" {
					leakedPath = b.ID()
				}
			}
		}
	}
	srv.mu.RUnlock()

	if leaked > 0 {
		t.Fatalf("Reload 之后还有 %d 个 Block 挂着正文缓存（例如 %s）；"+
			"它只在「扫描 → 索引」窗口里有价值，留着就是整库正文常驻内存", leaked, leakedPath)
	}
}

// 缓存命中时不能改变读到的内容：Body 有值就用它，没值退回 Read()。
func TestBlockBodyCacheDoesNotChangeContent(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const marker = "zzz-body-cache-marker"
	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:cached.md -- "+marker); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv.mu.RLock()
	var blk *memory.Block
	for _, tr := range srv.trees {
		if tr.Project != "blackegg/me" || tr.Branch != "fact" {
			continue
		}
		for i := range tr.Blocks {
			if tr.Blocks[i].Path == "cached.md" {
				b := tr.Blocks[i]
				blk = &b
			}
		}
	}
	srv.mu.RUnlock()
	if blk == nil {
		t.Fatal("找不到刚写的块")
	}

	// 走 Read()（Body 已清空）
	viaRead, err := srv.readBlock(*blk)
	if err != nil {
		t.Fatalf("readBlock via Read: %v", err)
	}
	// 走 Body
	withBody := *blk
	withBody.Body = viaRead
	viaBody, err := srv.readBlock(withBody)
	if err != nil {
		t.Fatalf("readBlock via Body: %v", err)
	}
	if string(viaRead) != string(viaBody) {
		t.Fatalf("两条路读到的内容不一致：Read=%q Body=%q", viaRead, viaBody)
	}
	if !contains(string(viaBody), marker) {
		t.Fatalf("读到的内容里没有刚写的标记：%q", viaBody)
	}
}
