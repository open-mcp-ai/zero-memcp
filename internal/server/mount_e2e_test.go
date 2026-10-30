package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// mintRestricted 签一张带 mount / cap 的凭证。
func mintRestricted(t *testing.T, dataDir string, c auth.Claims) string {
	t.Helper()
	secret, err := auth.LoadOrCreateSecret(filepath.Join(dataDir, "system", "jwt.key"))
	if err != nil {
		t.Fatal(err)
	}
	jti, err := auth.NewJTI()
	if err != nil {
		t.Fatal(err)
	}
	c.JTI = jti
	if c.Exp == 0 {
		c.Exp = time.Now().Add(time.Hour).Unix()
	}
	tok, err := auth.NewTokenStore(secret).Sign(c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func setupMountServer(t *testing.T) (*Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwxr-xr-x"

[[project]]
name  = "blackegg/daily"
owner = "zero"
mode  = "rwx------"

[[project]]
name  = "other/secret"
owner = "zero"
mode  = "rwx------"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	seed := func(proj, path, content string, mode gitstore.Mode) {
		r, err := gitstore.Init(filepath.Join(dataDir, "repos", proj+".git"))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Write("zero", path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit("zero", "init"); err != nil {
			t.Fatal(err)
		}
	}
	seed("blackegg/me", "SKILL.md", "我是 zero\n", gitstore.ModeRule)
	seed("blackegg/me", "01-shape.md", "形状\n", gitstore.ModeContent)
	seed("blackegg/daily", "today.md", "今天\n", gitstore.ModeContent)
	seed("other/secret", "x.md", "机密\n", gitstore.ModeContent)

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, dataDir
}

// 挂载表就是 chroot：表外的仓库完全不可见，表内用虚拟路径。
func TestMountIsChroot(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/"}},
	})

	// 表内：虚拟路径直接命中
	out, err := callMCP(t, srv, tok, "read 01-shape.md")
	if err != nil || !strings.Contains(out, "形状") {
		t.Fatalf("虚拟路径读失败: %v, out=%s", err, out)
	}

	// 表外：无论写什么形式都读不到机密
	for _, addr := range []string{"other/secret/x.md", "x.md"} {
		out, _ := callMCP(t, srv, tok, "read "+addr)
		if strings.Contains(out, "机密") {
			t.Fatalf("表外内容泄漏了（%s）: %s", addr, out)
		}
	}
}

// 多挂载点：/ 与 /shared 同时存在，长的优先。
func TestMountMultiplePoints(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero",
		Mount: []auth.MountPoint{
			{Project: "blackegg/me", At: "/"},
			{Project: "other/secret", At: "/shared"},
		},
	})

	out, err := callMCP(t, srv, tok, "read shared/x.md")
	if err != nil || !strings.Contains(out, "机密") {
		t.Fatalf("/shared 应映射到 other/secret: %v, out=%s", err, out)
	}
}

// 没有 x 能力 = 拿不到任何规则块（不注入）。
func TestCapWithoutXGetsNoRules(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rw"})

	body := httptestNewIdentity(srv, tok)
	if strings.Contains(body, "我是 zero") {
		t.Fatalf("没有 x 能力不该拿到规则块: %s", body)
	}
}

// 有 x 能力才注入。
func TestCapWithXGetsRules(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	body := httptestNewIdentity(srv, tok)
	if !strings.Contains(body, "我是 zero") {
		t.Fatalf("有 x 能力应注入规则块: %s", body)
	}
}

// cap 只能收窄：没有 w 就写不进去，即使 TOML 允许。
func TestCapCannotWiden(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "r"})

	_, err := callMCP(t, srv, tok, "note fact -- 想写的")
	if err == nil || !strings.Contains(err.Error(), "没有任何可写的仓库") {
		t.Fatalf("无 w 能力应被拒绝，实际: %v", err)
	}
}

// httptestNewIdentity 打一次 /identity，返回响应体。
func httptestNewIdentity(srv *Server, token string) string {
	req, _ := http.NewRequest(http.MethodGet, "/identity", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.handleIdentity(rec, req)
	return rec.Body.String()
}
