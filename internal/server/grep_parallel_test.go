package server

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// grep 现在并行读盘。并行改造最容易出的错是「结果顺序变了」——
// 同一个库搜两次给两种顺序，调用方会以为内容变了，或者
// 以为「上次在第一条的，这次跑到第三条了」。
//
// 这条测试钉住：输出按快照顺序，与串行版逐字节一致。
func TestGrepParallelKeepsStableOrder(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 写多条都含同一个标记的文件，顺序才有意义。
	const marker = "zzz-order-marker"
	for i := 0; i < 12; i++ {
		if _, err := callMCP(t, srv, tok,
			fmt.Sprintf("put blackegg/me@fact/ord/a%02d -- %s item-%02d", i, marker, i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	first, err := callMCP(t, srv, tok, "grep "+marker+" --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	// 连跑多次，顺序必须完全一样。
	for i := 0; i < 6; i++ {
		again, gerr := callMCP(t, srv, tok, "grep "+marker+" --scope blackegg/me@fact")
		if gerr != nil {
			t.Fatalf("grep run %d: %v", i, gerr)
		}
		if again != first {
			t.Fatalf("第 %d 次的输出与第一次不一致——并行改造把顺序打乱了\nfirst:\n%s\nalso:\n%s",
				i, first, again)
		}
	}

	// -c 的计数也必须稳定，且等于实际命中块数。
	cnt, err := callMCP(t, srv, tok, "grep -c "+marker+" --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("grep -c: %v", err)
	}
	if !strings.Contains(cnt, "12") {
		t.Fatalf("12 条命中却数出别的数：%s", cnt)
	}
}

// 并行读盘不能变成「读两遍」：命中块的正文要留住，排版阶段不能重读。
func TestGrepReadsEachBlockOnce(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const marker = "zzz-once-marker"
	for i := 0; i < 5; i++ {
		if _, err := callMCP(t, srv, tok,
			fmt.Sprintf("write blackegg/me@fact:once%d.md -- %s body %d", i, marker, i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// 把每个块的 Read 换成计数版：grep 一次，每块最多读一次。
	var reads atomic.Int64
	mutateTrees(t, srv, func(trees []*memory.Tree) {
		for _, tr := range trees {
			if tr.Project != "blackegg/me" || tr.Branch != "fact" {
				continue
			}
			for i := range tr.Blocks {
				orig := tr.Blocks[i].Read
				if orig == nil {
					continue
				}
				tr.Blocks[i].Read = func() ([]byte, error) {
					reads.Add(1)
					return orig()
				}
			}
		}
	})

	if _, err := callMCP(t, srv, tok, "grep "+marker+" --scope blackegg/me@fact"); err != nil {
		t.Fatalf("grep: %v", err)
	}

	var blocks int
	for _, tr := range testTrees(srv) {
		if tr.Project == "blackegg/me" && tr.Branch == "fact" {
			blocks += len(tr.Blocks)
		}
	}

	if got := reads.Load(); got > int64(blocks) {
		t.Fatalf("grep 读了 %d 次盘，而范围内只有 %d 个块——说明命中块被重读了", got, blocks)
	}

	_ = memory.MakeID
}
