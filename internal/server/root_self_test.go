package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// root 用户签一张只含单个仓库的 token，应当再也看不到别的仓库。
func TestRootCanLimitItself(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	// zero 是 root（配置里 Root: true），但 token 只挂 blackegg/me
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/"}},
	})

	// 自己能看
	out, err := callMCP(t, srv, tok, "read 01-shape.md")
	if err != nil || !strings.Contains(out, "形状") {
		t.Fatalf("挂载内应可读: %v, out=%s", err, out)
	}
	// 看不到 other/secret（虽然 TOML 里 root 有权）
	out, _ = callMCP(t, srv, tok, "read other/secret/x.md")
	if strings.Contains(out, "机密") {
		t.Fatalf("root token 的挂载表应生效，不该读到表外内容: %s", out)
	}
	// ls 也不该列出表外文件
	out, _ = callMCP(t, srv, tok, "ls")
	if strings.Contains(out, "x.md") {
		t.Fatalf("ls 不该列出表外仓库: %s", out)
	}
}
