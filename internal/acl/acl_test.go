package acl

import (
	"path/filepath"
	"testing"
)

func TestLookupPrecedence(t *testing.T) {
	rules := []Rule{
		{User: "zero-dev", Project: "BlackEggAI/zero", Branch: "*", Perm: "r"},
		{User: "zero-dev", Project: "BlackEggAI/zero", Branch: "main.shared", Perm: "rwx"},
		{User: "zero-dev", Project: "BlackEggAI/zero", Branch: "main.dev", Perm: "rw"},
	}

	// 精确分支必须命中自己的权限，不被通配 r 覆盖
	cases := []struct {
		branch   string
		wantPerm string
		wantHit  bool
	}{
		{"main.shared", "rwx", true},
		{"main.dev", "rw", true},
		{"main.chat", "r", true},  // 落在通配
		{"other", "r", true},      // 落在通配
	}
	for _, tc := range cases {
		p, ok := lookup(rules, "zero-dev", "BlackEggAI/zero", tc.branch)
		if ok != tc.wantHit || p != tc.wantPerm {
			t.Errorf("branch %q: got (%q, %v), want (%q, %v)", tc.branch, p, ok, tc.wantPerm, tc.wantHit)
		}
	}

	// 其他人完全没有
	if _, ok := lookup(rules, "someone", "BlackEggAI/zero", "main.shared"); ok {
		t.Errorf("someone should have no rules")
	}
}

func TestStorePersistenceAndAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "acl.json")

	s, err := Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if _, err := s.Put(Rule{User: "u1", Project: "p1", Branch: "b1", Perm: "rw"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.Put(Rule{User: "u1", Project: "p1", Branch: "*", Perm: "r"}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// 重新开一个，读出来应该一样
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	if perm, ok := s2.Lookup("u1", "p1", "b1"); !ok || perm != "rw" {
		t.Errorf("got %q, want rw", perm)
	}
	if perm, ok := s2.Lookup("u1", "p1", "other"); !ok || perm != "r" {
		t.Errorf("got %q, want r", perm)
	}

	// 覆盖 b1 为 rwx
	if _, err := s2.Put(Rule{User: "u1", Project: "p1", Branch: "b1", Perm: "rwx"}); err != nil {
		t.Fatalf("put overwrite: %v", err)
	}
	if len(s2.Rules()) != 2 {
		t.Errorf("expected 2 rules after overwrite, got %d", len(s2.Rules()))
	}

	// 删一条
	if err := s2.Delete("u1", "p1", "b1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(s2.Rules()) != 1 {
		t.Errorf("expected 1 rule after delete, got %d", len(s2.Rules()))
	}
}
