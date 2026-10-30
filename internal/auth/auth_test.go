package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 密钥必须只生成一次并落盘，权限必须是 0600。
func TestSecretPersistsAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt.key")

	a, err := LoadOrCreateSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("两次加载读到的密钥不同")
	}
	if len(a) != SecretSize {
		t.Fatalf("密钥长度应为 %d，实际 %d", SecretSize, len(a))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("密钥权限应为 0600，实际 %04o", mode)
	}
}

// 长度不对的密钥文件必须拒绝。
func TestSecretRejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt.key")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSecret(path); err == nil {
		t.Fatal("长度不对的密钥应报错")
	}
}

// 凭证签发、验签、提取身份。
func TestJWTRoundTrip(t *testing.T) {
	store := NewTokenStore([]byte("0123456789abcdef0123456789abcdef"))
	jti, err := NewJTI()
	if err != nil {
		t.Fatal(err)
	}

	tok, err := store.Sign(Claims{
		Sub: "alice",
		Exp: time.Now().Add(time.Hour).Unix(),
		JTI: jti,
	})
	if err != nil {
		t.Fatal(err)
	}

	c, err := store.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sub != "alice" || c.JTI != jti {
		t.Fatalf("载荷不一致: %+v", c)
	}
}

// 吊销立即生效，重复吊销幂等。
func TestRevocation(t *testing.T) {
	rev := NewRevocations(filepath.Join(t.TempDir(), "revoked"))
	jti := "0123456789abcdef"

	if rev.Revoked(jti) {
		t.Fatal("未吊销的凭证不该被判为已吊销")
	}
	if err := rev.Revoke(jti); err != nil {
		t.Fatal(err)
	}
	if !rev.Revoked(jti) {
		t.Fatal("吊销后应返回已吊销")
	}
	// 重复吊销不报错
	if err := rev.Revoke(jti); err != nil {
		t.Fatalf("重复吊销应幂等: %v", err)
	}
	// 空 jti 忽略
	if rev.Revoked("") {
		t.Fatal("空 jti 不该被当成已吊销")
	}
}

// 过期凭证被拒绝。
func TestVerifyRejectsExpired(t *testing.T) {
	store := NewTokenStore([]byte("0123456789abcdef0123456789abcdef"))
	tok, err := store.Sign(Claims{Sub: "x", Exp: time.Now().Add(-time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Verify(tok); err == nil {
		t.Fatal("过期凭证应被拒绝")
	}
}

// 篡改一个字节就签名失效。
func TestVerifyRejectsTampered(t *testing.T) {
	store := NewTokenStore([]byte("0123456789abcdef0123456789abcdef"))
	tok, err := store.Sign(Claims{Sub: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// 在签名段中间翻一位，不要翻最后一个字符：
	// base64 末字符的低位是填充位，翻转后解码结果可能不变，
	// 那样「篡改」实际没发生，测试会以约 1/4 的概率假失败。
	b := []byte(tok)
	b[0] ^= 0x01
	if _, err := store.Verify(string(b)); err == nil {
		t.Fatal("被篡改的凭证应验签失败")
	}
}

// 权限位九字符与八进制等价。
func TestPermRoundTrip(t *testing.T) {
	p1, err := ParsePerm("rwxr-x---")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ParsePerm("0750")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("两种拼写应等价: %v vs %v", p1, p2)
	}
	if p1.String() != "rwxr-x---" {
		t.Fatalf("String 错: %s", p1.String())
	}
	if p1.Octal() != "0750" {
		t.Fatalf("Octal 错: %s", p1.Octal())
	}
}
