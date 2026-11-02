package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// SessionStore 管 WebUI 的登录态。
//
// 为什么是内存而不是落盘：会话是「这次浏览器的这次登录」，
// 不是需要跨重启延续的身份。落盘会让「重启服务」失去
// 「把不该在线的人踢下线」这个作用，而这是运维最常用的一招。
//
// 为什么不用 JWT 当会话：JWT 无状态、签发后无法单独作废。
// 会话必须能随时撤销（改密、封禁、退出），这是 cookie + 服务端表
// 才做得到的事。MCP 那边用 JWT 是因为凭证本来就该是「发出去的纸」，
// 管理面恰恰相反。
type SessionStore struct {
	mu sync.Mutex
	// sessions 的键是 cookie 里的随机串，值是过期时间。
	sessions map[string]time.Time
	// ttl 是单次登录的最长有效期。
	ttl time.Duration
	// pending 表示当前口令还是引导口令：此时只允许访问改密接口。
	pending func() bool
}

// NewSessionStore 建一个会话表。pending 用来告诉它「是否必须改密」。
func NewSessionStore(ttl time.Duration, pending func() bool) *SessionStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionStore{
		sessions: map[string]time.Time{},
		ttl:      ttl,
		pending:  pending,
	}
}

// Create 新建一个会话，返回 cookie 值。
func (s *SessionStore) Create() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(s.ttl)
	s.mu.Unlock()
	return tok, nil
}

// Valid 报告 cookie 值是否是一个未过期的会话。
func (s *SessionStore) Valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, tok)
		return false
	}
	return true
}

// Drop 让一个会话立刻失效（退出登录）。
func (s *SessionStore) Drop(tok string) {
	s.mu.Lock()
	delete(s.sessions, tok)
	s.mu.Unlock()
}

// DropAll 清空全部会话。
//
// 改口令、改权限、改密钥之后必须调它：会话是「刚才验过身份」的凭据，
// 而身份相关的事实变了以后，那个「刚才」就不再成立。
func (s *SessionStore) DropAll() {
	s.mu.Lock()
	s.sessions = map[string]time.Time{}
	s.mu.Unlock()
}

// Count 返回当前有效会话数（含已过期但尚未清理的，仅用于展示）。
func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// TTL 返回会话有效期。
func (s *SessionStore) TTL() time.Duration { return s.ttl }
