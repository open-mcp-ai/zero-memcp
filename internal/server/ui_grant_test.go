package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// Gated capabilities and temporary grants.
//
// These tests pin the access boundary, not the UI:
//  1. A bit absent from cap cannot be granted at all.
//  2. A gated bit is denied until a matching grant exists.
//  3. A grant applies only to its project and branch.
//  4. Expiry and revocation take effect immediately.
//  5. Granting one bit does not grant the others.

// testCaller 造一个调用者。jti 必须走 NewJTI：授权按 JTI 落盘，
// 文件名直接来自它，所以非法字符会在写文件时被拒。
func testCaller(t *testing.T, cap string, mount []auth.MountPoint, gated string) Caller {
	t.Helper()
	jti, err := auth.NewJTI()
	if err != nil {
		t.Fatal(err)
	}
	return Caller{User: "zero", Cap: cap, Mount: NewMount(mount, "zero"), JTI: jti, Gated: gated}
}

func TestGatedBitIsDeniedUntilGranted(t *testing.T) {
	srv, _ := newWebUIServer(t)
	c := testCaller(t, "rw", []auth.MountPoint{{Project: "proj", At: "/", Cap: "rw"}}, "w")

	if srv.allowed(c, "proj", "zero", configBitWrite) {
		t.Fatal("a gated bit must be denied before a grant exists")
	}
	if !strings.Contains(srv.denyReason(c, "proj", "zero", configBitWrite), "not active") {
		t.Fatal("拒绝理由应当明确指出写权限未激活")
	}

	if err := srv.grants.Grant(auth.Grant{
		JTI: c.JTI, Project: "proj", Branch: "zero", Cap: "w",
		Expires: time.Now().Add(time.Hour).Unix(), GrantedBy: "test-human",
	}); err != nil {
		t.Fatal(err)
	}
	if !srv.allowed(c, "proj", "zero", configBitWrite) {
		t.Fatal("after a grant the branch should be writable")
	}
	if srv.allowed(c, "proj", "other", configBitWrite) {
		t.Fatal("a grant for branch zero must not allow branch other")
	}
	if srv.allowed(c, "other", "zero", configBitWrite) {
		t.Fatal("a grant for project proj must not allow project other")
	}
}

func TestGrantExpirationAndCredentialRevocation(t *testing.T) {
	srv, dataDir := newWebUIServer(t)
	c := testCaller(t, "rw", []auth.MountPoint{{Project: "proj", At: "/", Cap: "rw"}}, "w")

	if err := srv.grants.Grant(auth.Grant{
		JTI: c.JTI, Project: "proj", Branch: "zero", Cap: "w",
		Expires: time.Now().Add(-time.Second).Unix(), GrantedBy: "test-human",
	}); err != nil {
		t.Fatal(err)
	}
	if srv.allowed(c, "proj", "zero", configBitWrite) {
		t.Fatal("an expired grant must stop working immediately")
	}

	// Grant again, then revoke the credential: revocation outranks grants.
	if err := srv.grants.Grant(auth.Grant{
		JTI: c.JTI, Project: "proj", Branch: "zero", Cap: "w",
		GrantedBy: "test-human",
	}); err != nil {
		t.Fatal(err)
	}
	if !srv.allowed(c, "proj", "zero", configBitWrite) {
		t.Fatal("a non-expiring grant should apply")
	}
	rev := auth.NewRevocations(dataDir + "/system/revoked")
	if err := rev.Revoke(c.JTI); err != nil {
		t.Fatal(err)
	}
	// authenticate rejects revoked credentials before allowed() runs. Check the
	// grant record separately: revocation is its own layer, not a rewrite of it.
	if !srv.grants.Allowed(c.JTI, "proj", "zero", 'w') {
		t.Fatal("revocation must not rewrite the grant records themselves")
	}
}

func TestGrantCannotElevateCapability(t *testing.T) {
	srv, _ := newWebUIServer(t)
	c := testCaller(t, "r", []auth.MountPoint{{Project: "proj", At: "/", Cap: "r"}}, "w")
	if err := srv.grants.Grant(auth.Grant{
		JTI: c.JTI, Project: "proj", Branch: "zero", Cap: "w",
		GrantedBy: "test-human",
	}); err != nil {
		t.Fatal(err)
	}
	if srv.allowed(c, "proj", "zero", configBitWrite) {
		t.Fatal("a grant cannot add w to a credential whose cap lacks it")
	}
}

func TestWebUIMintAndGrantRequiresRecordedGrantPolicy(t *testing.T) {
	srv, _ := newWebUIServer(t)
	// Set a known password and log in, so the handlers run under a real session.
	if err := srv.passwords.Set("admin-password-x"); err != nil {
		t.Fatal(err)
	}
	cookie := loginUI(t, srv, "admin-password-x")
	h := srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true})

	body, _ := json.Marshal(map[string]any{
		"user": "zero", "name": "agent", "cap": "rw", "gated": "w",
		"expires_hours": 1,
		"mounts":        []map[string]string{{"project": "proj", "at": "/", "cap": "rw", "branch": "zero"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/ui/api/tokens/mint", strings.NewReader(string(body)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var minted struct {
		JTI   string `json:"jti"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil || minted.JTI == "" || minted.Token == "" {
		t.Fatalf("mint 响应缺少 token/JTI：%s (%v)", rec.Body.String(), err)
	}

	recs, errs := auth.LoadTokenRecords(srv.dataDir)
	if len(errs) > 0 {
		t.Fatalf("读取签发记录: %v", errs)
	}
	var found bool
	for _, tr := range recs {
		if tr.JTI == minted.JTI {
			found = true
			if tr.Gated != "w" || !tr.CapRecorded || !tr.MountRecorded || tr.Cap != "rw" {
				t.Fatalf("issuance record is missing audit fields: %+v", tr)
			}
		}
	}
	if !found {
		t.Fatal("no issuance record was written")
	}

	// A credential with no gated bits must refuse grants.
	plainJTI, err := auth.NewJTI()
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveTokenRecord(srv.dataDir, auth.TokenRecord{
		Name: "plain", JTI: plainJTI, Sub: "zero", Cap: "rw", CapRecorded: true,
		Mount: []auth.MountPoint{{Project: "proj", At: "/", Cap: "rw"}}, MountRecorded: true,
	}); err != nil {
		t.Fatal(err)
	}
	grantBody, _ := json.Marshal(map[string]any{"jti": plainJTI, "project": "proj", "branch": "zero", "cap": "w", "minutes": 10})
	req = httptest.NewRequest(http.MethodPost, "/ui/api/grants/grant", strings.NewReader(string(grantBody)))
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a credential without gated bits must refuse grants: %d %s", rec.Code, rec.Body.String())
	}
}

// A grant may only open a bit the credential really holds and really gates.
//
// The panel used to accept grants that could never take effect - the credential's
// own cap or mount table still denied the operation, so the UI showed "active"
// while every write failed. Rejecting at grant time is the only place the
// operator can see why.
func TestGrantRejectsBitsTheCredentialCannotUse(t *testing.T) {
	srv, _ := newWebUIServer(t)
	_ = srv.TakeBootstrapPassword()
	if err := srv.passwords.Set("grant-scope-pw"); err != nil {
		t.Fatal(err)
	}
	cookie := loginUI(t, srv, "grant-scope-pw")
	h := srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true})

	save := func(name, cap, gated string, mount []auth.MountPoint) string {
		jti, err := auth.NewJTI()
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.SaveTokenRecord(srv.dataDir, auth.TokenRecord{
			Name: name, JTI: jti, Sub: "zero", Cap: cap, Gated: gated,
			CapRecorded: true, Mount: mount, MountRecorded: true,
		}); err != nil {
			t.Fatal(err)
		}
		return jti
	}
	grant := func(jti, project, branch, cap string) (int, string) {
		body, _ := json.Marshal(map[string]any{"jti": jti, "project": project, "branch": branch, "cap": cap, "minutes": 5})
		req := httptest.NewRequest(http.MethodPost, "/ui/api/grants/grant", strings.NewReader(string(body)))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// cap has no w: a grant cannot invent a capability.
	noW := save("no-w", "r", "w", []auth.MountPoint{{Project: "proj", At: "/", Cap: "r"}})
	if code, body := grant(noW, "proj", "zero", "w"); code != http.StatusBadRequest {
		t.Fatalf("gated bit outside cap must be refused, got %d %s", code, body)
	} else if !strings.Contains(body, "exceeds credential cap") {
		t.Fatalf("refusal should name the cap, got %q", body)
	}

	// Nothing is gated on this credential: there is no gate to open.
	plain := save("plain", "rw", "", []auth.MountPoint{{Project: "proj", At: "/", Cap: "rw"}})
	if code, body := grant(plain, "proj", "zero", "w"); code != http.StatusBadRequest {
		t.Fatalf("credential with no gated bits must refuse grants, got %d %s", code, body)
	}

	// A bit that is gated but not being held back cannot be "released" either.
	rx := save("rx", "rwx", "x", []auth.MountPoint{{Project: "proj", At: "/", Cap: "rwx"}})
	if code, body := grant(rx, "proj", "zero", "w"); code != http.StatusBadRequest {
		t.Fatalf("granting an ungated bit must be refused, got %d %s", code, body)
	} else if !strings.Contains(body, "not gated") {
		t.Fatalf("refusal should say the bit is not gated, got %q", body)
	}

	// Project outside the mount table.
	mounted := save("mounted", "rw", "w", []auth.MountPoint{{Project: "proj", At: "/", Cap: "rw"}})
	if code, body := grant(mounted, "other", "zero", "w"); code != http.StatusBadRequest {
		t.Fatalf("project outside the mount table must be refused, got %d %s", code, body)
	} else if !strings.Contains(body, "mount table") {
		t.Fatalf("refusal should name the mount scope, got %q", body)
	}

	// Branch outside the mount entry.
	pinned := save("pinned", "rw", "w", []auth.MountPoint{{Project: "proj", At: "/", Branch: "zero", Cap: "rw"}})
	if code, body := grant(pinned, "proj", "elsewhere", "w"); code != http.StatusBadRequest {
		t.Fatalf("branch outside the mount entry must be refused, got %d %s", code, body)
	}

	// Mount cap narrower than the requested bit.
	narrow := save("narrow", "rwx", "rx", []auth.MountPoint{{Project: "proj", At: "/", Cap: "r"}})
	if code, body := grant(narrow, "proj", "zero", "x"); code != http.StatusBadRequest {
		t.Fatalf("bit above the mount cap must be refused, got %d %s", code, body)
	} else if !strings.Contains(body, "mount cap") {
		t.Fatalf("refusal should name the mount cap, got %q", body)
	}

	// The happy path still works.
	if code, body := grant(rx, "proj", "zero", "x"); code != http.StatusOK {
		t.Fatalf("a valid grant must succeed, got %d %s", code, body)
	}

	// Empty cap means unrestricted, not "no capability". A gated bit on such
	// a credential is still grantable unless the mount narrows it.
	unrestricted := save("unrestricted", "", "x", nil)
	if code, body := grant(unrestricted, "proj", "zero", "x"); code != http.StatusOK {
		t.Fatalf("empty cap means unrestricted, so the gated bit should be grantable: %d %s", code, body)
	}
}

// A grant opens exactly the bits it names - no more.
//
// This is what makes a capability set worth having instead of a single
// "allow write" switch: the same credential can be given read, or inject,
// or write, independently and temporarily.
func TestGrantOpensOnlyTheBitsItNames(t *testing.T) {
	srv, _ := newWebUIServer(t)
	c := testCaller(t, "rwx", []auth.MountPoint{{Project: "proj", At: "/", Cap: "rwx"}}, "rwx")

	for _, tc := range []struct {
		name string
		perm uint8
	}{
		{"r", configBitRead},
		{"w", configBitWrite},
		{"x", configBitExecute},
	} {
		if srv.allowed(c, "proj", "zero", tc.perm) {
			t.Fatalf("gated bit %s must start denied", tc.name)
		}
	}

	if err := srv.grants.Grant(auth.Grant{
		JTI: c.JTI, Project: "proj", Branch: "zero", Cap: "w", GrantedBy: "test-human",
	}); err != nil {
		t.Fatal(err)
	}
	if !srv.allowed(c, "proj", "zero", configBitWrite) {
		t.Fatal("the granted bit should be allowed")
	}
	if srv.allowed(c, "proj", "zero", configBitExecute) {
		t.Fatal("granting w must not open x")
	}
	if srv.allowed(c, "proj", "zero", configBitRead) {
		t.Fatal("granting w must not open r")
	}

	// A second grant on the same project/branch replaces the bit set, so
	// withdrawing part of it is possible without deleting the whole record.
	if err := srv.grants.Grant(auth.Grant{
		JTI: c.JTI, Project: "proj", Branch: "zero", Cap: "rx", GrantedBy: "test-human",
	}); err != nil {
		t.Fatal(err)
	}
	if !srv.allowed(c, "proj", "zero", configBitRead) {
		t.Fatal("the widened grant should allow reading")
	}
	if srv.allowed(c, "proj", "zero", configBitWrite) {
		t.Fatal("replacing the grant with rx must withdraw w again")
	}

	if n, err := srv.grants.Revoke(c.JTI, "proj", "zero"); err != nil || n != 1 {
		t.Fatalf("revoke should remove one grant, got %d %v", n, err)
	}
	if srv.allowed(c, "proj", "zero", configBitRead) {
		t.Fatal("after revoking, gated bits must be denied again")
	}
}

// The bootstrap password is one-time: reading it consumes it.
//
// A plain getter would keep it retrievable for the whole process lifetime,
// which defeats "printed once, then invalidated by the first password change".
func TestBootstrapPasswordIsSingleRead(t *testing.T) {
	srv, _ := newWebUIServer(t)
	first := srv.TakeBootstrapPassword()
	if first == "" {
		t.Fatal("first read should return the generated password")
	}
	if second := srv.TakeBootstrapPassword(); second != "" {
		t.Fatalf("second read must be empty, got %q", second)
	}
}

// 无限制凭证必须显式申请，且只发给 root 用户。
//
// 之前 WebUI 一律要求至少一个挂载，而 remote-key / remote-secret 又要求
// 空挂载表——两条路互锁，跨机同步的凭证（SSH 私钥 / HTTP token）根本
// 没法配。修法是允许显式申请，但三重约束缺一不可。
// 带一个非 root 用户的 WebUI 服务，用于验证「非 root 拿不到无限制凭证」。
func newWebUIServerWithGuest(t *testing.T) (*Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[user]]
name = "chat"

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

func TestWebUIMintUnrestrictedCredential(t *testing.T) {
	srv, _ := newWebUIServerWithGuest(t)
	if err := srv.passwords.Set("admin-password-x"); err != nil {
		t.Fatal(err)
	}
	cookie := loginUI(t, srv, "admin-password-x")
	h := srv.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true})
	mint := func(t *testing.T, body map[string]any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/ui/api/tokens/mint", strings.NewReader(string(b)))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	t.Run("缺显式声明则拒绝", func(t *testing.T) {
		code, msg := mint(t, map[string]any{"user": "zero", "name": "x", "cap": "rwx", "mounts": []map[string]string{}})
		if code != http.StatusBadRequest || !strings.Contains(msg, "unrestricted") {
			t.Fatalf("应提示显式声明 unrestricted: %d %s", code, msg)
		}
	})

	t.Run("非 root 用户拒绝", func(t *testing.T) {
		// 先声明一个非 root 用户，确保 cfgIndex 认识它
		code, msg := mint(t, map[string]any{"user": "chat", "name": "x", "cap": "rwx", "unrestricted": true, "mounts": []map[string]string{}})
		if code != http.StatusBadRequest || !strings.Contains(msg, "root user") {
			t.Fatalf("非 root 用户应被拒绝: %d %s", code, msg)
		}
	})

	t.Run("root + 显式声明 → 可用且真的无限制", func(t *testing.T) {
		code, msg := mint(t, map[string]any{"user": "zero", "name": "peer-admin", "cap": "rwx", "unrestricted": true, "mounts": []map[string]string{}})
		if code != http.StatusOK {
			t.Fatalf("应允许签发: %d %s", code, msg)
		}
		var out struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(msg), &out); err != nil || out.Token == "" {
			t.Fatalf("响应缺少 token: %s (%v)", msg, err)
		}
		// 用签出的凭证验证 rootUnrestricted 真的成立
		caller, err := srv.authenticate(reqWith(out.Token))
		if err != nil {
			t.Fatal(err)
		}
		if !caller.Root || !caller.Mount.Empty() {
			t.Fatalf("签出的凭证应为空挂载表 root: root=%v empty=%v", caller.Root, caller.Mount.Empty())
		}
		if !srv.rootUnrestricted(caller) {
			t.Fatalf("签出的凭证应通过 rootUnrestricted —— 否则 remote-key 仍然不可达")
		}
	})
}
