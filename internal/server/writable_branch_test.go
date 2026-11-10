package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 「读主干 + 写隔离分支」是分身边界的标准配置：
// 凭证挂 main:r + chat:rw，写入必须落到 chat，而不是配置默认分支 main。
func TestWritableBranchPicksWritableMount(t *testing.T) {
	m := NewMount([]auth.MountPoint{
		{Project: "BlackEggAI/zero", Branch: "main", Cap: "r"},
		{Project: "BlackEggAI/zero", Branch: "chat", Cap: "rw"},
	}, "main")
	if got := m.WritableBranch("BlackEggAI/zero"); got != "chat" {
		t.Fatalf("WritableBranch = %q, want chat", got)
	}
	if got := m.DefaultBranch(); got != "main" {
		t.Fatalf("DefaultBranch = %q, want main", got)
	}
}

// 没有任何可写分支时必须返回空，让调用方回落到原有逻辑。
func TestWritableBranchEmptyWhenReadOnly(t *testing.T) {
	m := NewMount([]auth.MountPoint{
		{Project: "BlackEggAI/zero", Branch: "main", Cap: "r"},
	}, "main")
	if got := m.WritableBranch("BlackEggAI/zero"); got != "" {
		t.Fatalf("WritableBranch = %q, want empty", got)
	}
}

// "*" 不是真实分支名，不能当落点。
func TestWritableBranchSkipsWildcard(t *testing.T) {
	m := NewMount([]auth.MountPoint{
		{Project: "BlackEggAI/zero", Branch: "*", Cap: "rw"},
	}, "main")
	if got := m.WritableBranch("BlackEggAI/zero"); got != "" {
		t.Fatalf("WritableBranch = %q, want empty", got)
	}
}
