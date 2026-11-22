package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 节点必须能同时看到两个上游的状态。
func TestTopologyShowsMultipleUpstreams(t *testing.T) {
	down, downData := setupMountServer(t)
	tokDown := mintRestricted(t, downData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 记录拉过两个不同的上游：
	//   hub-3060 是上游 A，拉到了 seq 106（caught up）
	//   backup   是上游 B，拉到了 seq 45、对方在 seq 50（behind 5）
	down.recordSync(syncRecord{
		Dir: "pull", Peer: "hub-3060", Node: "3060",
		Project: "blackegg/me", Branch: "zero",
		From: 0, To: 106, PeerHead: 106, Applied: 106,
	})
	down.recordSync(syncRecord{
		Dir: "pull", Peer: "backup-vps", Node: "vps",
		Project: "blackegg/me", Branch: "zero",
		From: 0, To: 45, PeerHead: 50, Applied: 45,
	})

	out, err := callMCP(t, down, tokDown, "sync")
	if err != nil {
		t.Fatal(err)
	}

	// 判据：
	// 1. upstream 区块包含两个不同的对端
	// 2. hub-3060 显示 caught up
	// 3. backup-vps 准确显示 behind 5
	if !strings.Contains(out, "hub-3060") || !strings.Contains(out, "backup-vps") {
		t.Fatalf("必须同时列出两个上游:\n%s", out)
	}
	if !strings.Contains(out, "behind 5") {
		t.Fatalf("必须准确报出第二个上游的落后条数 (behind 5):\n%s", out)
	}
	if !strings.Contains(out, "caught up") {
		t.Fatalf("必须报出第一个上游已追平:\n%s", out)
	}
}
