package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 新分支的第一次 put 在 commit 前只存在脏层，branch 列表也必须可见。
func TestBranchListsDirtyOnlyBranch(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	if _, err := callMCP(t, srv, tok, "put blackegg/me@feature -- 新分支的待提交内容"); err != nil {
		t.Fatal(err)
	}
	out, err := callMCP(t, srv, tok, "branch blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "feature") || !strings.Contains(out, "待提交") {
		t.Fatalf("branch 列表应显示待提交的 feature 分支，得到:\n%s", out)
	}
}
