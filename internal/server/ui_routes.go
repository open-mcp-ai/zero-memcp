package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// 管理面路由：只在声明了 webui = true 的端口上注册。
//
// 路由表本身就是边界。没开 webui 的端口上，这些路径不存在（404），
// 不是「存在但返回 403」——前者不告诉任何人这里有个管理面。

// uiMux 把管理面路由挂到给定 mux 上。
func (s *Server) uiMux(mux *http.ServeMux) {
	mux.HandleFunc("/ui/login", s.handleUILogin)
	mux.HandleFunc("/ui/logout", s.handleUILogout)
	mux.HandleFunc("/ui/password", s.handleUIPassword)
	mux.HandleFunc("/ui/api/", s.handleUIAPI)
}

// uiAuthGuard 是管理面 API 的统一入口检查。
//
// 必须改密的会话只能访问改密接口——这是让「随机初始口令打印在日志里」
// 变得可接受的唯一前提：那个值在第一次登录后当场作废。
func (s *Server) uiAuthGuard(w http.ResponseWriter, r *http.Request, allowPending bool) bool {
	if s.sessions == nil || s.passwords == nil {
		http.Error(w, "admin panel is not initialized", http.StatusServiceUnavailable)
		return false
	}
	// 同源校验先做：跨站请求连口令都不该看到提交结果。
	if err := s.checkUISameOrigin(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return false
	}
	if hasBearerCredential(r) {
		http.Error(w, "admin panel does not accept MCP credentials; sign in with a browser", http.StatusForbidden)
		return false
	}
	if !s.sessions.Valid(uiSession(r)) {
		http.Error(w, "not signed in or session expired", http.StatusUnauthorized)
		return false
	}
	if !allowPending && s.passwords.MustChange() {
		http.Error(w, "the initial password is still active; change it first", http.StatusForbidden)
		return false
	}
	return true
}

// handleUILogin 校验口令并建立会话。
func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.passwords == nil || s.sessions == nil {
		http.Error(w, "admin panel is not initialized", http.StatusServiceUnavailable)
		return
	}
	if err := s.checkUISameOrigin(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	// 登录接口本身必须拒绝 Bearer：否则「用 token 换会话」就成立，
	// 管理面的隔离当场失效。
	if hasBearerCredential(r) {
		http.Error(w, "login endpoint does not accept MCP credentials", http.StatusForbidden)
		return
	}
	if s.uiLimiter == nil {
		s.uiLimiter = newUILoginLimiter()
	}
	ip := clientIP(r)
	if ok, wait := s.uiLimiter.allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		http.Error(w, "too many login attempts; try again later", http.StatusTooManyRequests)
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !s.passwords.Verify(body.Password) {
		locked := s.uiLimiter.fail(ip)
		msg := "incorrect password"
		if locked {
			msg = "incorrect password; temporarily locked after repeated failures"
		}
		http.Error(w, msg, http.StatusUnauthorized)
		return
	}
	s.uiLimiter.reset(ip)
	tok, err := s.sessions.Create()
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	setUISessionCookie(w, r, tok, s.sessions.TTL())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":            true,
		"must_change":   s.passwords.MustChange(),
		"session_count": s.sessions.Count(),
	})
}

// handleUILogout 丢弃当前会话。
func (s *Server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.sessions != nil {
		if tok := uiSession(r); tok != "" {
			s.sessions.Drop(tok)
		}
	}
	clearUISessionCookie(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleUIPassword 修改管理口令。
func (s *Server) handleUIPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 改密接口允许「必须改密」状态下访问——否则初始口令永远换不掉。
	if !s.uiAuthGuard(w, r, true) {
		return
	}
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !s.passwords.Verify(body.Old) {
		http.Error(w, "incorrect current password", http.StatusUnauthorized)
		return
	}
	if len(body.New) < 8 {
		http.Error(w, "new password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	if body.New == body.Old {
		http.Error(w, "new password must differ from the current password", http.StatusBadRequest)
		return
	}
	if err := s.passwords.Set(body.New); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 改口令后清空全部会话：别的浏览器里开着的管理台必须掉线。
	// 这正是「口令泄露了，改一个就好」能真正生效的原因。
	s.sessions.DropAll()
	tok, err := s.sessions.Create()
	if err != nil {
		http.Error(w, "failed to recreate session", http.StatusInternalServerError)
		return
	}
	setUISessionCookie(w, r, tok, s.sessions.TTL())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "must_change": false})
}

// handleUIAPI 是管理面的数据接口。
func (s *Server) handleUIAPI(w http.ResponseWriter, r *http.Request) {
	if !s.uiAuthGuard(w, r, false) {
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/ui/api/")
	switch action {
	case "overview":
		s.uiOverview(w, r)
	case "tokens":
		s.uiTokens(w, r)
	case "tokens/mint":
		s.uiTokenMint(w, r)
	case "tokens/revoke":
		s.uiTokenRevoke(w, r)
	case "grants":
		s.uiGrants(w, r)
	case "grants/grant":
		s.uiGrantAdd(w, r)
	case "grants/revoke":
		s.uiGrantRevoke(w, r)
	case "search":
		s.uiSearch(w, r)
	case "gc":
		s.uiGC(w, r)
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
}

// uiJSON 统一写 JSON 响应。
func uiJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// uiSessionTTLText 供页面展示。
func (s *Server) uiSessionTTLText() string {
	if s.sessions == nil {
		return "-"
	}
	return s.sessions.TTL().String()
}
