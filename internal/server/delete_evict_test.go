package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 删除留证超过上限时，必须严格淘汰最旧的，且 status 必须如实报出被淘汰数量。
//
// 群内评审第 4 条：
//
//	删除留证早先有 100 条硬上限，且靠 sync.Map.Range 遍历随机删，
//	被丢的不一定是最旧的，账就此断流。
//
// 现在的行为：严格按 At 时间淘汰最旧的；上限提至 1000；status 明确显示被轮转数量。
func TestDeleteConflictStrictFIFOEviction(t *testing.T) {
	srv, data := setupMountServer(t)
	token := mintRestricted(t, data, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 为了快速测试，把上限临时填满：灌入超过上限的删除留证
	// 留证时间单调递增
	baseTime := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	const total = workDeleteConflictKeep + 50 // 超过 50 条

	// 与生产顺序一致：recordWorkConflict 先淘汰、再写入。
	for i := 0; i < total; i++ {
		path := fmt.Sprintf("notes/del_%04d.md", i)
		srv.evictOldDeleteConflict()
		srv.workSync.conflicts.Store(workConflictKey("p/one", "main", "peer-a", path), workConflict{
			Path: path, LocalSHA: "sha1", Peer: "peer-a", Kind: ConflictDelete,
			At: baseTime.Add(time.Duration(i) * time.Second),
		})
	}

	// 1. 内存中保留的总数必须恰好是 workDeleteConflictKeep
	var kept []workConflict
	srv.workSync.conflicts.Range(func(_, v any) bool {
		if c, ok := v.(workConflict); ok && c.Kind == ConflictDelete {
			kept = append(kept, c)
		}
		return true
	})
	if len(kept) != workDeleteConflictKeep {
		t.Fatalf("保留数量应为 %d，实际 %d", workDeleteConflictKeep, len(kept))
	}

	// 2. 被淘汰的必须是最旧的前 50 条（即 del_0000 到 del_0049）
	// 最老的一条应该是 del_0050
	seen := map[string]bool{}
	for _, c := range kept {
		seen[c.Path] = true
	}
	for i := 0; i < 50; i++ {
		oldPath := fmt.Sprintf("notes/del_%04d.md", i)
		if seen[oldPath] {
			t.Fatalf("最旧的条目 %s 应当已被淘汰，实际仍在保留列表中", oldPath)
		}
	}
	for i := 50; i < total; i++ {
		newPath := fmt.Sprintf("notes/del_%04d.md", i)
		if !seen[newPath] {
			t.Fatalf("较新的条目 %s 必须保留，实际丢失", newPath)
		}
	}

	// 3. deleteEvicted 计数必须精确等于 50
	if ev := srv.workSync.deleteEvicted.Load(); ev != 50 {
		t.Fatalf("淘汰计数应为 50，实际 %d", ev)
	}

	// 4. status 输出里必须能看到这个数字
	out, err := callMCP(t, srv, token, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "50 older historical evidence rotated to daemon.log") {
		t.Fatalf("status 必须报告被轮转的历史证据数，实际输出:\n%s", out)
	}
}
