package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 这一组测试钉的是管理面与 Agent 通道的**双向**隔离。
//
// 核心承诺只有一句：管理台能改权限，所以它的凭据绝不能是 Agent 手上那张纸。
// 反过来，Agent 通道也不接受浏览器会话。两个方向都必须拒绝，
// 只做一个方向等于把门装反。

// newWebUIServer 起一个开了管理面的实例。
func newWebUIServer(t *testing.T) (*Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name  = "proj"
owner = "zero"
mode  = "rwx------"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := gitstore.Init(filepath.Join(dataDir, "repos", "proj.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "a.md", []byte("内容"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
		WebUI:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, dataDir
}

// loginUI 走一遍登录，返回会话 cookie。
func loginUI(t *testing.T, srv *Server, pw string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": pw})
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == uiSessionCookie {
			return c
		}
	}
	t.Fatal("登录成功但没有下发会话 cookie")
	return nil
}

// 管理面必须拒绝 MCP 的 Bearer 凭证。
//
// 这是整套权限模型的根：管理台能签发凭证、能放权。
// 如果 Agent 手里那张 token 也能进管理面，那它就能给自己发一张更宽的，
// 「凭证只能收窄」当场作废。
func TestUIRejectsBearerCredential(t *testing.T) {
	srv, dataDir := newWebUIServer(t)
	pw := srv.TakeBootstrapPassword()
	if pw == "" {
		t.Fatal("首次启动应当生成初始口令")
	}
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	for _, path := range []string{"/ui/api/overview", "/ui/api/tokens", "/ui/api/grants"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 带 Bearer 应当 403，得到 %d（%s）", path, rec.Code, rec.Body.String())
		}
	}

	// 登录接口本身也不能用 token 换会话。
	body, _ := json.Marshal(map[string]string{"password": pw})
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("登录接口带 Bearer 应当 403，得到 %d", rec.Code)
	}
}

// MCP 通道必须拒绝 WebUI 会话。
func TestMCPRejectsUISession(t *testing.T) {
	srv, dataDir := newWebUIServer(t)
	pw := srv.TakeBootstrapPassword()
	// 首次登录必须改密，否则管理接口一律 403。
	cookie := loginUI(t, srv, pw)
	if err := srv.passwords.Set("a-long-enough-password"); err != nil {
		t.Fatal(err)
	}
	cookie = loginUI(t, srv, "a-long-enough-password")

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	_ = tok

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":{"cmd":"status"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("MCP 通道不该接受 WebUI 会话 cookie")
	}
	if !strings.Contains(rec.Body.String(), "WebUI sessions") {
		t.Errorf("拒绝理由应说清是「会话不是 MCP 凭据」，得到 %q", rec.Body.String())
	}

	// The boundary remains exclusive even when both credentials are present.
	// A valid Bearer must not turn a browser session request into an MCP call.
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "without an admin cookie") {
		t.Fatalf("MCP must reject session cookie even alongside Bearer: %d %s", rec.Code, rec.Body.String())
	}
}

// 未启用 webui 的端口上，管理面路由必须不存在（404），而不是 403。
//
// 结构性不存在不告诉任何人「这里有个管理台」，而 403 等于确认了它的存在。
func TestUIRoutesAbsentWithoutWebUI(t *testing.T) {
	srv, _ := newWebUIServer(t)
	h := srv.muxFor(ListenBlock{Allow: []string{"*"}}) // 注意：没有 WebUI
	for _, path := range []string{"/", "/ui/login", "/ui/api/overview", "/api"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 在未开 webui 的端口上应当 404，得到 %d", path, rec.Code)
		}
	}
}

// 初始口令必须强制更换。
//
// 它是随机生成并打印到日志的，所以「一次性」是它唯一的安全性来源：
// 不强制改密，它就是一个写在 docker logs 里的常驻密码。
func TestUIBootstrapPasswordMustChange(t *testing.T) {
	srv, _ := newWebUIServer(t)
	pw := srv.TakeBootstrapPassword()
	if pw == "" {
		t.Fatal("应当生成初始口令")
	}
	if !srv.passwords.MustChange() {
		t.Fatal("初始口令必须被标记为「必须改密」")
	}

	cookie := loginUI(t, srv, pw)
	// 改密前，数据接口应当拒绝。
	req := httptest.NewRequest(http.MethodGet, "/ui/api/tokens", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("初始口令未改密前，数据接口应当 403，得到 %d", rec.Code)
	}

	// 改密之后放行。
	body, _ := json.Marshal(map[string]string{"old": pw, "new": "brand-new-password"})
	req = httptest.NewRequest(http.MethodPost, "/ui/password", strings.NewReader(string(body)))
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("改密失败: %d %s", rec.Code, rec.Body.String())
	}
	if srv.passwords.MustChange() {
		t.Fatal("改密后不应再要求改密")
	}
	if srv.passwords.Verify(pw) {
		t.Fatal("旧口令必须失效")
	}
}

// 改密必须让所有旧会话掉线。
//
// 「口令泄露了，改一个就好」这句话成立的前提就是这条：
// 否则攻击者手里那个会话在改密之后依然有效，改密等于没做。
func TestUIPasswordChangeDropsAllSessions(t *testing.T) {
	srv, _ := newWebUIServer(t)
	_ = srv.TakeBootstrapPassword()
	if err := srv.passwords.Set("first-password-x"); err != nil {
		t.Fatal(err)
	}
	old := loginUI(t, srv, "first-password-x")

	body, _ := json.Marshal(map[string]string{"old": "first-password-x", "new": "second-password-y"})
	req := httptest.NewRequest(http.MethodPost, "/ui/password", strings.NewReader(string(body)))
	req.AddCookie(old)
	rec := httptest.NewRecorder()
	srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("改密失败: %d %s", rec.Code, rec.Body.String())
	}
	if srv.sessions.Valid(old.Value) {
		t.Fatal("改密后旧会话必须失效")
	}
}

// 跨站请求必须被拒（Origin 与 Host 不一致）。
func TestUIRejectsCrossOrigin(t *testing.T) {
	srv, _ := newWebUIServer(t)
	_ = srv.TakeBootstrapPassword()
	if err := srv.passwords.Set("cross-origin-pw"); err != nil {
		t.Fatal(err)
	}
	cookie := loginUI(t, srv, "cross-origin-pw")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/tokens", nil)
	req.Host = "127.0.0.1:1729"
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true}).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站请求应当 403，得到 %d", rec.Code)
	}
}

// 登录失败要限速：argon2id 故意很慢，不限制就是一个免费的 CPU 打满器。
func TestUILoginRateLimited(t *testing.T) {
	srv, _ := newWebUIServer(t)
	_ = srv.TakeBootstrapPassword()
	if err := srv.passwords.Set("rate-limit-pw"); err != nil {
		t.Fatal(err)
	}
	h := srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true})
	var last int
	for i := 0; i < 8; i++ {
		body, _ := json.Marshal(map[string]string{"password": "wrong"})
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(string(body)))
		req.RemoteAddr = "10.0.0.9:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("连续失败后应当 429，得到 %d", last)
	}
}

// Single-port deployment always serves the admin surface.
//
// In a container or an npm wrapper there are no CLI flags and the user never
// sees an interactive prompt. The admin surface must be there out of the box
// on the only exposed port, so the operator can open http://host:port/ and
// set their password on the first run.
func TestSinglePortAlwaysServesAdminSurface(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		DataDir:    dataDir,
		ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:     "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if !srv.UIRoutesEnabled() {
		t.Fatal("UIRoutesEnabled should be true for single-port deployments")
	}
	if pw := srv.TakeBootstrapPassword(); pw == "" {
		t.Fatal("the admin surface must bootstrap a password on cold start")
	}

	h := srv.Mux()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin route should exist and demand a session, got %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("index HTML should be 200, got %d", rec2.Code)
	}
}

// Multi-port configuration: if no port asks for the admin surface, the first
// port gets it automatically so the instance is never left without one.
func TestMultiPortWithoutExplicitWebUIAssignsToFirstPort(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name = "proj"
owner = "zero"
mode = "rwx------"

[[listen]]
addr = "127.0.0.1:19901"
allow = ["*"]

[[listen]]
addr = "127.0.0.1:19902"
allow = ["read"]
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		DataDir:    dataDir,
		ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if len(srv.listens) != 2 {
		t.Fatalf("expected 2 listeners, got %d", len(srv.listens))
	}
	if !srv.listens[0].WebUI {
		t.Fatal("first port should have WebUI enabled as fallback")
	}
	if srv.listens[1].WebUI {
		t.Fatal("second port should NOT have WebUI enabled")
	}

	urls := srv.AdminURLs()
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:19901/" {
		t.Fatalf("AdminURLs should report the first port, got %v", urls)
	}
}

// Multi-port configuration: an explicit webui = true is respected, and ports
// without it stay 404.
func TestMultiPortExplicitWebUIIsolation(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name = "proj"
owner = "zero"
mode = "rwx------"

[[listen]]
addr = "127.0.0.1:19911"
allow = ["read"]

[[listen]]
addr = "127.0.0.1:19912"
allow = ["*"]
webui = true
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		DataDir:    dataDir,
		ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if srv.listens[0].WebUI {
		t.Fatal("first port should NOT have WebUI (second was explicit)")
	}
	if !srv.listens[1].WebUI {
		t.Fatal("second port should have WebUI")
	}

	urls := srv.AdminURLs()
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:19912/" {
		t.Fatalf("AdminURLs should report the explicit port, got %v", urls)
	}
}
