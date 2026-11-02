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

// 身份由凭证在签发时定死，不由请求头切换。
//
// 这条测试盯的是「身份不是鉴权机制」这个边界：一张凭证的 cap 与挂载表
// 就是它全部的世界，任何请求头都不该让这个世界变大。想换视角就换凭证。

// 一张凭证一个视角：挂载表决定看得见什么。
func TestCredentialDefinesSingleView(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	// 只挂 daily 的凭证。
	dailyOnly := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Cap:   "rw",
		Mount: []auth.MountPoint{{Project: "blackegg/daily", At: "/"}},
	})

	out, err := callMCP(t, srv, dailyOnly, "read today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("应能读挂载内的 daily: %v, out=%s", err, out)
	}
	// 挂载外的东西完全不可见。
	out, _ = callMCP(t, srv, dailyOnly, "read 01-shape.md")
	if strings.Contains(out, "形状") {
		t.Fatalf("不该看到挂载外的 blackegg/me: %s", out)
	}

	// 想要另一个视角，就再签一张——而不是给同一张加 header。
	meAndDaily := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero",
		Cap: "rw",
		Mount: []auth.MountPoint{
			{Project: "blackegg/me", At: "/"},
			{Project: "blackegg/daily", At: "/daily", Cap: "r"},
		},
	})
	out, err = callMCP(t, srv, meAndDaily, "read 01-shape.md")
	if err != nil || !strings.Contains(out, "形状") {
		t.Fatalf("第二张凭证应能读 blackegg/me: %v, out=%s", err, out)
	}
	out, err = callMCP(t, srv, meAndDaily, "read daily/today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("第二张凭证应能读 daily: %v, out=%s", err, out)
	}
}

// 请求头不能改变身份：带什么 header 都还是同一张凭证的世界。
func TestRoleHeaderHasNoEffect(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Cap:   "r",
		Mount: []auth.MountPoint{{Project: "blackegg/daily", At: "/"}},
	})

	// 带任意角色 header，都仍然使用同一张凭证的身份与挂载范围。
	for _, role := range []string{"admin", "root", "zero", "dev", "owner"} {
		req := httptest.NewRequest(http.MethodGet, "/identity", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Zero-Role", role)
		c, err := srv.authenticate(req)
		if err != nil {
			t.Fatalf("header %q 不该导致鉴权失败: %v", role, err)
		}
		// 世界没变：仍然只有 daily。
		if !c.Mount.Visible("blackegg/daily", "zero") {
			t.Errorf("header %q 改变了可见范围", role)
		}
		if c.Mount.Visible("blackegg/me", "zero") {
			t.Errorf("header %q 越权看到了挂载外项目", role)
		}
	}
}

// 同一张凭证内，挂载点 cap 生效：写自己的仓库可以，写别处不行。
func TestPerMountCap(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero",
		Mount: []auth.MountPoint{
			{Project: "blackegg/me", At: "/", Cap: "rw"},
			{Project: "blackegg/daily", At: "/daily", Cap: "r"},
		},
	})

	if _, err := callMCP(t, srv, tok, "put -- 写进 me"); err != nil {
		t.Fatalf("挂载点 cap=rw 应能写: %v", err)
	}

	out, err := callMCP(t, srv, tok, "read daily/today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("挂载点 cap=r 应能读: %v, out=%s", err, out)
	}
}

// intersectCap 只能收窄，且空集不能退化成「不限制」。
//
// 为什么单独测 r∩w：空串在 hasCap 里的语义是「全允许」，
// 所以不相交的两个能力集如果返回空串，等于提权（已实测：
// 只读凭证 + 只写挂载点 = 能写）。空集必须用 capNothing 表达。
func TestIntersectCap(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "r", "r"},
		{"rw", "", "rw"},
		{"rwx", "rw", "rw"},
		{"r", "rwx", "r"},
		{"r", "w", capNothing},
		{"r", "wx", capNothing},
		{"rwx", "rwx", "rwx"},
	}
	for _, c := range cases {
		if got := intersectCap(c.a, c.b); got != c.want {
			t.Errorf("intersectCap(%q,%q) = %q，要 %q", c.a, c.b, got, c.want)
		}
	}
	// 空集对每一位都必须判假，且不能被当成「不限制」
	for _, bit := range []rune{'r', 'w', 'x'} {
		if hasCap(capNothing, bit) {
			t.Errorf("capNothing 不应含有能力位 %c", bit)
		}
	}
}

// 凭证只能收窄，不能放宽：TOML 里给某个用户配了只读，
// 即使它的凭证 cap=rwx，写入也必须被配置层拦下。
func TestConfigLayerNarrowsCredential(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[user]]
name = "reader"

[[project]]
name  = "blackegg/daily"
owner = "zero"
mode  = "rwx------"

[[access]]
user    = "reader"
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
	defer srv.Close()

	// reader 的凭证自身给了 rwx，但配置层只给 r。
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "reader",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/daily", At: "/"}},
	})

	out, err := callMCP(t, srv, tok, "read today.md")
	if err != nil || !strings.Contains(out, "今天") {
		t.Fatalf("reader 应能读 daily: %v, out=%s", err, out)
	}

	if _, err := callMCP(t, srv, tok, "put -- 试写"); err == nil {
		t.Fatal("配置层只给 r，写入必须被拒绝（凭证不能放宽配置）")
	}
	if _, err := callMCP(t, srv, tok, "put blackegg/daily -- 试写"); err == nil {
		t.Fatal("显式指定目标也必须被配置层拒绝")
	}
}

// recall 必须按命中自己所在的分支判权限。
//
// 踩过的坑：handleRecall 一律拿 s.defaultBranch 去判，于是「不在默认分支上」
// 的命中被权限层静默过滤。症状是受限身份 put 写进去、read 读得到、
// log 看得到，recall 却永远无命中——不报错，只是搜不到。
func TestRecallUsesHitBranch(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	// 这张凭证挂的是 daily 的 chat 分支，不是默认分支。
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero",
		Cap: "rwx",
		Mount: []auth.MountPoint{
			{Project: "blackegg/daily", At: "/", Branch: "chat", Cap: "rw"},
		},
	})

	if _, err := callMCP(t, srv, tok, "put -- 群友张三喜欢 Rust"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	out, err := callMCP(t, srv, tok, "recall 张三 Rust")
	if err != nil {
		t.Fatalf("recall 失败: %v", err)
	}
	// 挂载点是 /，虚拟地址里不带项目名，只看有没有命中内容。
	if strings.Contains(out, "no matches") || !strings.Contains(out, ".md") {
		t.Fatalf("应该能 recall 到自己写的记录，实际: %q", out)
	}
}
