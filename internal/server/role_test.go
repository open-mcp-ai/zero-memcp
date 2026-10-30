package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 调一次工具，带上角色 header。
func callWithRole(t *testing.T, srv *Server, token, role, cmd string) (string, error) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":{"cmd":"` + cmd + `"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+token)
	if role != "" {
		req.Header.Set("X-Zero-Role", role)
	}
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		return "", errString(strings.TrimSpace(rec.Body.String()))
	}
	return mcpText(t, rec.Body.Bytes())
}

type errString string

func (e errString) Error() string { return string(e) }

// 一张凭证带多个角色，按 header 切换；每个角色看到的世界不同。
func TestRolesSwitchByHeader(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:     "zero",
		Default: "chat",
		Roles: map[string]auth.Role{
			"chat": {
				Cap:   "rw",
				Mount: []auth.MountPoint{{Project: "blackegg/daily", At: "/"}},
			},
			"dev": {
				Cap: "rw",
				Mount: []auth.MountPoint{
					{Project: "blackegg/me", At: "/"},
					{Project: "blackegg/daily", At: "/daily", Cap: "r"},
				},
			},
		},
	})

	// chat 角色：只有 daily
	out, err := callWithRole(t, srv, tok, "chat", "read today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("chat 角色应能读 daily: %v, out=%s", err, out)
	}
	// chat 角色看不到 me
	out, _ = callWithRole(t, srv, tok, "chat", "read 01-shape.md")
	if strings.Contains(out, "形状") {
		t.Fatalf("chat 角色不该看到 blackegg/me: %s", out)
	}

	// dev 角色：能看到 me，也能读 daily
	out, err = callWithRole(t, srv, tok, "dev", "read 01-shape.md")
	if err != nil || !strings.Contains(out, "形状") {
		t.Fatalf("dev 角色应能读 blackegg/me: %v, out=%s", err, out)
	}
	out, err = callWithRole(t, srv, tok, "dev", "read daily/today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("dev 角色应能读 daily: %v, out=%s", err, out)
	}

	// 没指定角色 → 用 Default(chat)
	out, err = callWithRole(t, srv, tok, "", "read today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("默认角色应生效: %v, out=%s", err, out)
	}
}

// 未知角色必须拒绝，不能回落到默认。
func TestUnknownRoleRejected(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:     "zero",
		Default: "chat",
		Roles: map[string]auth.Role{
			"chat": {Cap: "r", Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/"}}},
		},
	})
	_, err := callWithRole(t, srv, tok, "admin", "read 01-shape.md")
	if err == nil || !strings.Contains(err.Error(), "未知角色") {
		t.Fatalf("未知角色应被拒绝，实际: %v", err)
	}
}

// 定义了角色但没默认、请求也不给 → 拒绝，不猜。
func TestRolesWithoutDefaultRejected(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero",
		Roles: map[string]auth.Role{
			"chat": {Cap: "r", Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/"}}},
		},
	})
	_, err := callWithRole(t, srv, tok, "", "read 01-shape.md")
	if err == nil || !strings.Contains(err.Error(), "没指定") {
		t.Fatalf("无默认角色时应拒绝，实际: %v", err)
	}
}

// 同一张凭证内，挂载点 cap 生效：写自己的仓库可以，写 daily 不行。
func TestPerMountCap(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero",
		Mount: []auth.MountPoint{
			{Project: "blackegg/me", At: "/", Cap: "rw"},
			{Project: "blackegg/daily", At: "/daily", Cap: "r"},
		},
	})

	if _, err := callMCP(t, srv, tok, "note fact -- 写进 me"); err != nil {
		t.Fatalf("挂载点 cap=rw 应能写: %v", err)
	}

	out, err := callMCP(t, srv, tok, "read daily/today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("挂载点 cap=r 应能读: %v, out=%s", err, out)
	}
}

// intersectCap 只能收窄。
func TestIntersectCap(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "r", "r"},
		{"rw", "", "rw"},
		{"rwx", "rw", "rw"},
		{"r", "rwx", "r"},
		{"r", "w", ""},
		{"rwx", "rwx", "rwx"},
	}
	for _, c := range cases {
		if got := intersectCap(c.a, c.b); got != c.want {
			t.Errorf("intersectCap(%q,%q) = %q，要 %q", c.a, c.b, got, c.want)
		}
	}
}

// TOML 里配了 [[access]] user="chat" perm="r"，
// 即使凭证来自 sub="zero"(root)，切到 chat 角色写 daily 也必须被拦下。
func TestRoleNarrowsByTomlAccess(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[user]]
name = "chat"

[[project]]
name  = "blackegg/daily"
owner = "zero"
mode  = "rwx------"

[[access]]
user    = "chat"
project = "blackegg/daily"
perm    = "r"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	r, err := gitstore.Init(filepath.Join(dataDir, "repos", "blackegg/daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Write("zero", "today.md", []byte("今天\n"), gitstore.ModeContent)
	_, _ = r.Commit("zero", "init")

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:     "zero",
		Default: "chat",
		Roles: map[string]auth.Role{
			"chat": {
				Cap:   "rwx",
				Mount: []auth.MountPoint{{Project: "blackegg/daily", At: "/"}},
			},
		},
	})

	// chat 读：允许
	out, err := callWithRole(t, srv, tok, "chat", "read today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("chat 角色应能读 daily: %v, out=%s", err, out)
	}

	// chat 写：TOML 明确只给 r，即使 sub=zero 也必须被拦下
	_, err = callWithRole(t, srv, tok, "chat", "note fact -- 试写")
	if err == nil {
		t.Fatal("chat 角色只有 r 权限，写 daily 必须被拒绝")
	}
	if !strings.Contains(err.Error(), "没有任何可写的仓库") {
		t.Fatalf("应拒绝写入并提示无可写仓库，实际: %v", err)
	}

	// 显式指定 blackegg/daily 也必须被拒绝，并说清缺少 w
	_, err = callWithRole(t, srv, tok, "chat", "note fact blackegg/daily -- 试写")
	if err == nil {
		t.Fatal("显式指定目标也必须被拒绝")
	}
	if !strings.Contains(err.Error(), "chat") || !strings.Contains(err.Error(), "缺少 w") {
		t.Fatalf("显式指定拒绝理由应指明角色缺少 w，实际: %v", err)
	}
}

// recall 必须按命中自己所在的分支判权限。
//
// 踩过的坑：handleRecall 一律拿 s.persona 去判，于是「不在人格分支上」
// 的命中被权限层静默过滤。症状是 chat 角色 note 写进去、read 读得到、
// log 看得到，recall 却永远无命中——不报错，只是搜不到。
func TestRecallUsesHitBranch(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero",
		Roles: map[string]auth.Role{
			"chat": {Cap: "rwx", Mount: []auth.MountPoint{
				{Project: "blackegg/daily", At: "/", Branch: "chat", Cap: "rw"},
			}},
		},
	})

	// chat 角色写一条自己的记录
	if _, err := callWithRole(t, srv, tok, "chat", "note fact -- 群友张三喜欢 Rust"); err != nil {
		t.Fatalf("chat 写入失败: %v", err)
	}

	out, err := callWithRole(t, srv, tok, "chat", "recall 张三 Rust")
	if err != nil {
		t.Fatalf("recall 失败: %v", err)
	}
	// 挂载点是 /，虚拟地址里不带项目名，只看有没有命中内容。
	if strings.Contains(out, "无命中") || !strings.Contains(out, ".md") {
		t.Fatalf("chat 应该能 recall 到自己写的记录，实际: %q", out)
	}
}
