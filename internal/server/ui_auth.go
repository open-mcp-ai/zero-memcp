package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebUI 的鉴权与 MCP 完全分开。
//
// 为什么必须分开，而不是「同一个 authenticate 加个分支」：
//
//	MCP 的凭证是 Bearer JWT，是发给 Agent 的纸。管理面如果也认它，
//	那 Agent 手里的 token 就等于管理台钥匙——而管理台能改权限、
//	能吊销凭证、能看审计。这不是「多给一点权限」，是把整套权限模型
//	的根交给被约束的一方。
//
//	所以两边是**互斥**的：管理面只认 session cookie，见到 Bearer 一律拒绝；
//	MCP 通道不认 session cookie。判据是「凭据形态」，不是「来源地址」——
//	同一台机器上跑的东西也能发 HTTP 请求，地址从来不是身份。

// uiSessionCookie 是管理面会话 cookie。
const uiSessionCookie = "zmcp_ui"

// uiAuthError 表示管理面鉴权失败。
type uiAuthError struct {
	Code int
	Msg  string
}

func (e *uiAuthError) Error() string { return e.Msg }

// uiSession 返回当前请求的管理面会话值（可能为空）。
func uiSession(r *http.Request) string {
	c, err := r.Cookie(uiSessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// hasBearerCredential 报告请求是否带了任何 MCP 形态的凭证。
//
// 这里刻意比 authenticate 更宽：只要出现 Bearer 头、?token=、
// 或 Basic 口令，就认为「这是 Agent 通道的请求」，管理面一律拒绝。
// 宁可在有人混用时直接报错，也不要悄悄按 cookie 放行——
// 那会让「我的脚本为什么能改权限」变成一个没人查得出的谜。
func hasBearerCredential(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return true
	}
	if r.Header.Get("Authorization") != "" {
		return true
	}
	if r.URL.Query().Get("token") != "" {
		return true
	}
	if _, _, ok := r.BasicAuth(); ok {
		return true
	}
	return false
}

// checkUISameOrigin 校验 Origin/Host 一致。
func (s *Server) checkUISameOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// 没有 Origin 的请求（同源导航、curl）靠 cookie + SameSite 守。
		// 但带 Referer 的跨站提交仍要拦。
		if ref := r.Header.Get("Referer"); ref != "" {
			u, err := url.Parse(ref)
			if err != nil {
				return &uiAuthError{Code: http.StatusForbidden, Msg: "could not parse Referer"}
			}
			if !sameHost(u.Host, r.Host) {
				return &uiAuthError{Code: http.StatusForbidden, Msg: "cross-origin request denied"}
			}
		}
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return &uiAuthError{Code: http.StatusForbidden, Msg: "could not parse Origin"}
	}
	if !sameHost(u.Host, r.Host) {
		return &uiAuthError{Code: http.StatusForbidden, Msg: "cross-origin request denied"}
	}
	return nil
}

// sameHost 比较两个 host:port，容忍端口缺省。
func sameHost(a, b string) bool {
	ha, pa := splitHostPortLoose(a)
	hb, pb := splitHostPortLoose(b)
	if ha != hb {
		return false
	}
	if pa == "" || pb == "" {
		return true
	}
	return pa == pb
}

func splitHostPortLoose(v string) (host, port string) {
	if h, p, err := net.SplitHostPort(v); err == nil {
		return strings.ToLower(h), p
	}
	return strings.ToLower(v), ""
}

// uiLoginLimiter 限制登录失败速率。
//
// 为什么必须有：管理面口令是唯一屏障，而 argon2id 校验故意很慢
// （每次约 40ms、64MB）。不限制的话，一个脚本能把 CPU 和内存打满，
// 顺便做在线爆破。这里按「连续失败」计数，成功即清零。
type uiLoginLimiter struct {
	mu      sync.Mutex
	fails   map[string]*loginFail
	maxTry  int
	lockout time.Duration
}

type loginFail struct {
	count int
	until time.Time
}

func newUILoginLimiter() *uiLoginLimiter {
	return &uiLoginLimiter{
		fails:   map[string]*loginFail{},
		maxTry:  5,
		lockout: 5 * time.Minute,
	}
}

// allow 报告某个来源现在是否可以尝试登录。
func (l *uiLoginLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.fails[ip]
	if !ok {
		return true, 0
	}
	if time.Now().Before(f.until) {
		return false, time.Until(f.until)
	}
	return true, 0
}

// fail 记一次失败，返回是否已进入锁定。
func (l *uiLoginLimiter) fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.fails[ip]
	if !ok {
		f = &loginFail{}
		l.fails[ip] = f
	}
	f.count++
	if f.count >= l.maxTry {
		f.count = 0
		f.until = time.Now().Add(l.lockout)
		return true
	}
	return false
}

// reset 在登录成功后清掉该来源的失败计数。
func (l *uiLoginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

// clientIP 取请求来源地址（不含端口）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// setUISessionCookie 写会话 cookie。
//
// HttpOnly 防 XSS 偷 cookie；SameSite=Strict 让跨站请求根本带不上它；
// Path=/ 因为管理面就在根上。Secure 只在请求已经是 https 时加——
// 本地 http 上加 Secure 会让 cookie 直接被浏览器丢掉。
func setUISessionCookie(w http.ResponseWriter, r *http.Request, tok string, ttl time.Duration) {
	c := &http.Cookie{
		Name:     uiSessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(ttl.Seconds()),
	}
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

// clearUISessionCookie 清掉会话 cookie。
func clearUISessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     uiSessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// uiForbiddenIfBearer rejects admin sessions on the MCP route.
//
// The reverse half of the isolation: a request carrying the WebUI cookie is
// refused even when it also carries a valid Bearer token. Presence of the
// admin cookie alone is enough to make it a management request, so the caller
// is told to use one channel at a time instead of silently succeeding.
func uiForbiddenIfBearer(w http.ResponseWriter, r *http.Request) bool {
	if uiSession(r) == "" {
		return false
	}
	// Reject the WebUI cookie even when a valid Bearer token is also present.
	//
	// The two channels must be mutually exclusive, not merely "Bearer wins".
	// Otherwise a browser request carrying both credentials could silently
	// cross the boundary, making it impossible to tell whether a successful MCP
	// call came from the agent credential or the human admin session.
	http.Error(w, fmt.Sprintf("MCP endpoint does not accept WebUI sessions (%s); use Authorization: Bearer <jwt> without an admin cookie", uiSessionCookie),
		http.StatusUnauthorized)
	return true
}
