package auth

import (
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	ss := NewSessionStore(time.Hour, func() bool { return false })

	tok, err := ss.Create()
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Fatal("会话 cookie 不应为空")
	}
	if !ss.Valid(tok) {
		t.Fatal("刚创建的会话应当有效")
	}
	if ss.Valid("") {
		t.Fatal("空 cookie 必须无效")
	}
	if ss.Valid("never-issued") {
		t.Fatal("未签发过的 cookie 必须无效")
	}

	// 两次 Create 必须给出不同的 token（否则会话会互相顶掉）
	tok2, err := ss.Create()
	if err != nil {
		t.Fatal(err)
	}
	if tok == tok2 {
		t.Fatal("两次创建会话给出了相同的 token")
	}

	ss.Drop(tok)
	if ss.Valid(tok) {
		t.Fatal("Drop 之后会话应当立刻失效")
	}
	if !ss.Valid(tok2) {
		t.Fatal("Drop 一个会话不应影响另一个")
	}
}

func TestSessionExpiry(t *testing.T) {
	// 负数 TTL 会被规范成默认值，所以直接构造一个极短 TTL
	ss := NewSessionStore(time.Nanosecond, func() bool { return false })
	tok, err := ss.Create()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if ss.Valid(tok) {
		t.Fatal("过期会话必须无效")
	}
}

// DropAll 是「改口令 / 改权限 / 改密钥之后清场」的那一下。
func TestSessionDropAll(t *testing.T) {
	ss := NewSessionStore(time.Hour, func() bool { return false })
	var toks []string
	for i := 0; i < 5; i++ {
		tok, err := ss.Create()
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, tok)
	}
	if got := ss.Count(); got < 5 {
		t.Fatalf("应有至少 5 个会话，实际 %d", got)
	}

	ss.DropAll()
	for _, tok := range toks {
		if ss.Valid(tok) {
			t.Fatal("DropAll 之后所有会话都必须失效")
		}
	}
}
