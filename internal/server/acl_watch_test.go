package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/acl"
	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 分支级授权：精确分支必须压过通配，且不受声明顺序影响。
//
// 这是「加一条共享分支」能不重签凭证的前提——权限表能表达
// 「整个项目只读 + 这一条分支可写」。
func TestACLRuleBeatsWildcard(t *testing.T) {
	dir := t.TempDir()
	store, err := acl.Open(filepath.Join(dir, "acl.json"))
	if err != nil {
		t.Fatalf("open acl: %v", err)
	}
	// 故意把通配写在后面：如果实现按「最后一条生效」，这条会把
	// main.shared 的 rwx 吃掉，测试就会红。
	if _, err := store.Put(acl.Rule{User: "zero-dev", Project: "p", Branch: "main.shared", Perm: "rwx"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(acl.Rule{User: "zero-dev", Project: "p", Perm: "r"}); err != nil {
		t.Fatal(err)
	}

	if perm, ok := store.Lookup("zero-dev", "p", "main.shared"); !ok || perm != "rwx" {
		t.Fatalf("main.shared: got (%q,%v), want (rwx,true)", perm, ok)
	}
	if perm, ok := store.Lookup("zero-dev", "p", "main.chat"); !ok || perm != "r" {
		t.Fatalf("main.chat should fall back to wildcard r, got (%q,%v)", perm, ok)
	}
}

// ACL 命中时，凭证挂载表里没有这条分支也必须放行——这是
// 「不用重签凭证」的全部意义。
// ACL 不能越过凭证天花板（挂载表）。
//
// 这条测试原先断言的是相反的行为（"ACL 授予的 main.shared 应当可写，
// 即使凭证没挂这条分支"）。语义统一成「token 是选择范围，其它层只能在
// 里面挑」之后，那个行为就是一条绕过通道：一张只挂了 branch-a 的 token
// 被 acl put 开了 branch-b 之后就能读 branch-b。
//
// 想保留「随时给凭证开新分支」的便利，把分支（或 "*"）写进挂载表——
// 那是这张凭证本来就该声明的意图，而不是事后由另一张表替它扩大。
func TestACLCannotExceedCredentialMounts(t *testing.T) {
	dir := t.TempDir()
	store, err := acl.Open(filepath.Join(dir, "acl.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(acl.Rule{User: "zero-dev", Project: "BlackEggAI/zero", Branch: "main.shared", Perm: "rwx"}); err != nil {
		t.Fatal(err)
	}

	s := &Server{acl: store, defaultBranch: "main", projects: NewProjectStore(t.TempDir())}
	// 凭证只挂了 main.dev
	c := Caller{
		User:     "zero", // 与主体同一用户：ACL 按身份区分，不按用户
		Identity: "zero-dev",
		Cap:      "rwx",
		Mount:    NewMount([]auth.MountPoint{{Project: "BlackEggAI/zero", At: "/", Branch: "main.dev", Cap: "rwx"}}, "main"),
	}

	if s.allowed(c, "BlackEggAI/zero", "main.shared", configBitWrite) {
		t.Fatal("ACL 授予的 main.shared 不得越过凭证挂载表（天花板在最底层拦）")
	}
	if s.branchVisible(c, "BlackEggAI/zero", "main.shared") {
		t.Fatal("branchVisible 与 allowed 必须同一口径：看不见的分支不能靠 ACL 变可见")
	}
	// 没被 ACL 授予、也不在挂载表里的分支仍然不可见
	if s.allowed(c, "BlackEggAI/zero", "main.chat", configBitRead) {
		t.Fatal("main.chat 既没挂载也没授权，必须拒绝")
	}

	// 天花板之内，ACL 仍然是有效的一层：挂载表覆盖全部分支时，
	// 由 ACL 决定「哪几条开、开多大」。
	c2 := Caller{
		User:     "zero",
		Identity: "zero-dev",
		Cap:      "rwx",
		Mount:    NewMount([]auth.MountPoint{{Project: "BlackEggAI/zero", At: "/", Branch: "*"}}, "main"),
	}
	if !s.allowed(c2, "BlackEggAI/zero", "main.shared", configBitWrite) {
		t.Fatal("天花板覆盖该分支时，ACL 的授权应当生效")
	}
	if !s.branchVisible(c2, "BlackEggAI/zero", "main.shared") {
		t.Fatal("天花板覆盖该分支时，ACL 授予的分支应算可见")
	}
	// 天花板之内 ACL 依然能收窄：给它 r，它就写不进去（cap 明明有 w）。
	if _, err := store.Put(acl.Rule{User: "zero-dev", Project: "BlackEggAI/zero", Branch: "main.chat", Perm: "r"}); err != nil {
		t.Fatal(err)
	}
	if !s.allowed(c2, "BlackEggAI/zero", "main.chat", configBitRead) {
		t.Fatal("ACL 给了 r，读应当放行")
	}
	if s.allowed(c2, "BlackEggAI/zero", "main.chat", configBitWrite) {
		t.Fatal("ACL 只给了 r，即使 cap 有 w 也必须拒绝")
	}
}

// cap 仍是天花板：ACL 给 rwx，但凭证只有 r 就写不进去。
func TestACLCannotExceedCredentialCap(t *testing.T) {
	dir := t.TempDir()
	store, _ := acl.Open(filepath.Join(dir, "acl.json"))
	if _, err := store.Put(acl.Rule{User: "zero-dev", Project: "p", Branch: "b", Perm: "rwx"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{acl: store, defaultBranch: "main", projects: NewProjectStore(t.TempDir())}
	c := Caller{User: "zero", Identity: "zero-dev", Cap: "r", Mount: NewMount(nil, "main")}

	if s.allowed(c, "p", "b", configBitWrite) {
		t.Fatal("凭证 cap 只有 r 时，ACL 的 rwx 不该放行写")
	}
	if !s.allowed(c, "p", "b", configBitRead) {
		t.Fatal("读应当放行")
	}
}

// 关注表必须跨重启存活：否则两个分身重启后就不再关注共享分支，
// 对讲机静默失效。
func TestTreeWatchPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watches.json")

	h, err := newPersistentTreeWatchHub(path)
	if err != nil {
		t.Fatal(err)
	}
	h.Add("zero", TreeWatch{Project: "BlackEggAI/zero", Branch: "main.shared", Since: "abc123"}, false)
	h.Add("zero-dev", TreeWatch{Project: "BlackEggAI/zero", Branch: "main.shared", Since: "abc123"}, false)

	// 模拟进程重启
	h2, err := newPersistentTreeWatchHub(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"zero", "zero-dev"} {
		list := h2.List(id)
		if len(list) != 1 {
			t.Fatalf("%s: expected 1 watch after restart, got %d", id, len(list))
		}
		if list[0].Branch != "main.shared" || list[0].Since != "abc123" {
			t.Fatalf("%s: watch not restored: %+v", id, list[0])
		}
	}

	// 重新锚定同一棵树不该产生第二条
	h2.Add("zero", TreeWatch{Project: "BlackEggAI/zero", Branch: "main.shared", Since: "def456"}, false)
	if got := len(h2.List("zero")); got != 1 {
		t.Fatalf("re-anchor should replace, got %d watches", got)
	}

	// Clear 也要落盘
	h2.Clear("zero-dev")
	h3, err := newPersistentTreeWatchHub(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(h3.List("zero-dev")); got != 0 {
		t.Fatalf("cleared watch came back after restart: %d", got)
	}
	if got := len(h3.List("zero")); got != 1 {
		t.Fatalf("other identity's watch lost: %d", got)
	}
}

func TestTreeWatchHubWithoutPathDoesNotTouchDisk(t *testing.T) {
	h := newTreeWatchHub()
	h.Add("x", TreeWatch{Project: "p", Branch: "b"}, false)
	if got := len(h.List("x")); got != 1 {
		t.Fatalf("in-memory hub should still work, got %d", got)
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "watches.json")); err == nil {
		t.Fatal("in-memory hub must not write a file")
	}
	_ = time.Now()
}

// ACL 授予的分支必须能被列举出来（log/ls 用的是虚拟路径，
// 未挂载分支没有虚拟路径 —— 早先因此整个分支在列举里消失）。
func TestVirtualPathFallsBackForACLBranches(t *testing.T) {
	dir := t.TempDir()
	store, _ := acl.Open(filepath.Join(dir, "acl.json"))
	if _, err := store.Put(acl.Rule{User: "zero-dev", Project: "p", Branch: "main.shared", Perm: "rwx"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{acl: store, defaultBranch: "main", projects: NewProjectStore(t.TempDir())}
	c := Caller{
		User:     "zero",
		Identity: "zero-dev",
		Cap:      "rwx",
		Mount:    NewMount([]auth.MountPoint{{Project: "p", At: "/", Branch: "main.dev", Cap: "rwx"}}, "main"),
	}

	// 挂载表内的分支：虚拟路径（chroot 视角）
	if got := s.virtualPath(c, "p", "main.dev", "a/b.md"); got != "a/b.md" {
		t.Fatalf("mounted branch virtual path = %q, want a/b.md", got)
	}
	// 未被挂载表覆盖的分支：即使 ACL 写着允许，也必须返回空串（token 天花板）。
	if got := s.virtualPath(c, "p", "main.shared", "a/b.md"); got != "" {
		t.Fatalf("unmounted branch must stay invisible even with ACL, got %q", got)
	}
	// 挂载表只写了项目或通配（Branch="*"）但没有显式虚拟路径映射时，
	// 才由 branchVisible 放行并退回真实地址。
	if got := s.virtualPath(c, "p", "main.chat", "a/b.md"); got != "" {
		t.Fatalf("unrelated branch virtual path = %q, want empty", got)
	}
}
