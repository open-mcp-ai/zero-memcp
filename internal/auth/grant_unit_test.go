package auth

import (
	"testing"
	"time"
)

func TestGrantLifecycle(t *testing.T) {
	dir := t.TempDir()
	gs := NewGrantStore(dir)

	// 授予 1 小时 rw
	if err := gs.Grant(Grant{
		JTI:     "0123456789abcdef",
		Project: "proj-x",
		Branch:  "fact",
		Cap:     "rw",
		Expires: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	if !gs.Allowed("0123456789abcdef", "proj-x", "fact", 'r') {
		t.Fatal("应当允许 r")
	}
	if !gs.Allowed("0123456789abcdef", "proj-x", "fact", 'w') {
		t.Fatal("应当允许 w")
	}
	if gs.Allowed("0123456789abcdef", "proj-x", "fact", 'x') {
		t.Fatal("不应当允许未授予的 x")
	}
	if gs.Allowed("0123456789abcdef", "other-proj", "fact", 'r') {
		t.Fatal("不应当允许未授权的项目")
	}
	if gs.Allowed("0123456789abcdef", "proj-x", "other-branch", 'r') {
		t.Fatal("不应当允许未授权的分支")
	}

	all, err := gs.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(all))
	}

	// 撤销
	n, err := gs.Revoke("0123456789abcdef", "proj-x", "fact")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应撤销 1 条，实际 %d", n)
	}
	if gs.Allowed("0123456789abcdef", "proj-x", "fact", 'r') {
		t.Fatal("撤销后不应再允许访问")
	}
}

func TestGrantExpiration(t *testing.T) {
	gs := NewGrantStore(t.TempDir())

	// 已过期
	if err := gs.Grant(Grant{
		JTI: "fedcba9876543210", Project: "p", Branch: "b", Cap: "r",
		Expires: time.Now().Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if gs.Allowed("fedcba9876543210", "p", "b", 'r') {
		t.Fatal("已过期的 grant 应当被拒绝")
	}
}

// 通配分支 "*" 覆盖该项目下所有分支。
func TestGrantWildcardBranch(t *testing.T) {
	gs := NewGrantStore(t.TempDir())
	if err := gs.Grant(Grant{
		JTI: "aaaabbbbccccdddd", Project: "p", Branch: "*", Cap: "r",
		Expires: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, br := range []string{"fact", "main", "anything"} {
		if !gs.Allowed("aaaabbbbccccdddd", "p", br, 'r') {
			t.Fatalf("通配分支应当覆盖 %q", br)
		}
	}
}

// 授权必须持久化：重启后仍然有效（它是「临时放行」，不该因进程重启失效）。
func TestGrantPersistsAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	gs1 := NewGrantStore(dir)
	if err := gs1.Grant(Grant{
		JTI: "1111222233334444", Project: "p", Branch: "b", Cap: "r",
		Expires: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	gs2 := NewGrantStore(dir)
	if !gs2.Allowed("1111222233334444", "p", "b", 'r') {
		t.Fatal("授权应当跨实例持久化")
	}
}

// JTI 必须是 16 位十六进制 —— 这是凭证编号的格式约束。
//
// 它不只是「格式好看」：授权绑定 JTI，而 JTI 来自 JWT 的 jti 声明。
// 允许任意字符串就意味着授权表里会出现无法与真实凭证对应的键，
// 那种记录永远匹配不上，也不会被任何撤销操作清掉——静默膨胀。
func TestGrantRejectsMalformedJTI(t *testing.T) {
	gs := NewGrantStore(t.TempDir())
	for _, bad := range []string{"jti-1", "", "short", "gggggggggggggggg", "0123456789abcde"} {
		if err := gs.Grant(Grant{JTI: bad, Project: "p", Branch: "b", Cap: "r"}); err == nil {
			t.Fatalf("畸形 JTI %q 应当被拒绝", bad)
		}
	}
}
