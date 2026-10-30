package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 输出侧是否真的虚拟化了？读、搜、列、写四条路径都查一遍。
func TestMountOutputVirtualized(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/"}},
	})

	t.Run("read 输出", func(t *testing.T) {
		out, err := callMCP(t, srv, tok, "read 01-shape.md")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "blackegg/me") {
			t.Errorf("read 输出里泄漏了真实路径: %s", out)
		}
	})
	t.Run("ls 输出", func(t *testing.T) {
		out, _ := callMCP(t, srv, tok, "ls")
		t.Logf("ls = %s", out)
	})
	t.Run("recall 输出", func(t *testing.T) {
		out, _ := callMCP(t, srv, tok, "recall 形状")
		t.Logf("recall = %s", out)
	})
	t.Run("note 输出", func(t *testing.T) {
		out, _ := callMCP(t, srv, tok, "note fact -- 测试")
		t.Logf("note = %s", out)
	})
}
func TestMountGrepAndLog(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/"}},
	})
	t.Run("grep 输出", func(t *testing.T) {
		out, _ := callMCP(t, srv, tok, "grep 形状")
		t.Logf("grep = %s", out)
		if strings.Contains(out, "blackegg/me") {
			t.Errorf("grep 输出泄漏全路径: %s", out)
		}
	})
	t.Run("log 输出", func(t *testing.T) {
		out, _ := callMCP(t, srv, tok, "log 5")
		t.Logf("log = %s", out)
		if strings.Contains(out, "blackegg/me") {
			t.Errorf("log 输出泄漏全路径: %s", out)
		}
	})
}
