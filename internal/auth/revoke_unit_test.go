package auth

import "testing"

func TestNewJTIFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		jti, err := NewJTI()
		if err != nil {
			t.Fatal(err)
		}
		// 生成的 JTI 必须能通过自己的校验
		if err := ValidateJTI(jti); err != nil {
			t.Fatalf("NewJTI 生成了不合法的 JTI %q: %v", jti, err)
		}
		if seen[jti] {
			t.Fatalf("重复的 JTI：%s", jti)
		}
		seen[jti] = true
	}
}

func TestRevocationLifecycle(t *testing.T) {
	r := NewRevocations(t.TempDir())
	jti, err := NewJTI()
	if err != nil {
		t.Fatal(err)
	}

	if r.Revoked(jti) {
		t.Fatal("未撤销的 JTI 不应报告为已撤销")
	}
	if err := r.Revoke(jti); err != nil {
		t.Fatal(err)
	}
	if !r.Revoked(jti) {
		t.Fatal("撤销后应当报告为已撤销")
	}

	list, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range list {
		if x == jti {
			found = true
		}
	}
	if !found {
		t.Fatalf("List 里应当包含 %s，实际 %v", jti, list)
	}

	// 恢复：撤销是可以在误操作后收回的
	if err := r.Unrevoke(jti); err != nil {
		t.Fatal(err)
	}
	if r.Revoked(jti) {
		t.Fatal("Unrevoke 之后不应再报告为已撤销")
	}
}

// 派生凭证撤销时，父凭证也要被记住——否则用父凭证就能重新派生一张绕过。
func TestRevocationParentChain(t *testing.T) {
	r := NewRevocations(t.TempDir())
	parent, err := NewJTI()
	if err != nil {
		t.Fatal(err)
	}
	child, err := NewJTI()
	if err != nil {
		t.Fatal(err)
	}

	if err := r.RecordParent(child, parent); err != nil {
		t.Fatal(err)
	}
	// 撤销父凭证应当连带子凭证失效（见 Revoked 的注释）
	if err := r.Revoke(parent); err != nil {
		t.Fatal(err)
	}
	if !r.Revoked(child) {
		t.Fatal("父凭证撤销后，其派生凭证也必须失效")
	}
}
