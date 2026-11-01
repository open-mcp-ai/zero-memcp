package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 这一组测的是「权限矩阵」，不是功能：
// root 能做什么、用什么凭证做的、以及同一张 root 凭证被缩窄之后还剩什么。
//
// 它守的是一条容易悄悄破掉的性质：
//
//	root 是「管理身份」，不是「绕过凭证」。
//
// root-only 的动词需要 Root，但 token 的 cap 与挂载表仍然是它的天花板。
// 任何一处只查 Root、不查 cap/mount，就会让一张本该只读的
// 应急凭证变成万能钥匙。

// rootAdminVerbs 是 help 里声明为「只有维护记忆结构时才需要」的那一组。
var rootAdminVerbs = []string{
	"branch",
	"tag",
	"cp",
	"mv",
	"delete",
	"archive",
	"reset",
	"batch",
	"commit",
	"write",
	"config",
}

// 非 root 身份碰任何一个管理动词都必须被拒绝。
func TestRootAdminVerbsRejectNonRoot(t *testing.T) {
	srv, _ := setupMountServer(t)
	nonRoot := Caller{User: "bob", Identity: "bob"}

	for _, verb := range rootAdminVerbs {
		if _, err := srv.execTerm(verb+" blackegg/me", Viewer{}, nonRoot); err == nil {
			t.Errorf("%s 应只对 root 开放，实际通过了", verb)
		}
	}
	for _, verb := range []string{"reload", "users"} {
		if _, err := srv.execTerm(verb, Viewer{}, nonRoot); err == nil {
			t.Errorf("%s 应只对 root 开放，实际通过了", verb)
		}
	}
}

// 缩窄到单个仓库的 root 凭证，不得用管理动词碰挂载表外的仓库。
func TestRootAdminVerbsRespectMount(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:  "zero",
		Name: "narrow",
		Cap:  "rwx",
		// 只挂 blackegg/me 全部分支；blackegg/daily 与 other/secret 都在表外。
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*"}},
	})
	caller, err := srv.authenticate(reqWith(tok))
	if err != nil {
		t.Fatal(err)
	}
	if !caller.Root {
		t.Fatal("凭证主体是 zero(root)，应保留 Root 身份")
	}

	// 改结构的动作必须直接报错，不能「成功但什么也没做」。
	outside := []string{
		"tag other/secret v1 zero",
		"commit other/secret@zero",
		"delete other/secret@zero/x.md",
		"archive other/secret@zero",
		"cp other/secret@zero/x.md -> blackegg/me@zero/copied.md",
		"mv other/secret@zero/x.md -> blackegg/me@zero/moved.md",
		"write other/secret@zero/x.md -- 覆盖",
	}
	for _, line := range outside {
		if _, err := callMCP(t, srv, tok, line); err == nil {
			t.Errorf("挂载表外的管理操作应被拒绝: %s", line)
		}
	}

	// 列结构的动作不能泄漏表外项目的存在。
	for _, line := range []string{"branch blackegg/daily", "branch"} {
		out, err := callMCP(t, srv, tok, line)
		if err != nil {
			continue // 直接报错也是可接受的答案
		}
		if strings.Contains(out, "blackegg/daily") || strings.Contains(out, "other/secret") {
			t.Errorf("%q 泄漏了挂载表外的项目: %s", line, out)
		}
	}

	// 挂载表内的仓库仍然可以管理：建分支、打 tag、写文件。
	if _, err := callMCP(t, srv, tok, "branch blackegg/me feature"); err != nil {
		t.Fatalf("挂载表内应可建分支: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "tag blackegg/me v1 zero"); err != nil {
		t.Fatalf("挂载表内应可打 tag: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/note.md -- 正文"); err != nil {
		t.Fatalf("挂载表内应可写: %v", err)
	}
}

// 挂载表点名到具体分支时，不得凭空造出表外的新分支。
func TestScopedBranchCannotCreateOutsideBranch(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "single",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero"}},
	})
	if _, err := callMCP(t, srv, tok, "branch blackegg/me shadow"); err == nil {
		t.Fatal("只挂 zero 分支的凭证不该能新建 shadow 分支")
	}
}

// 只读 root 凭证不得写入、不得提交、不得改配置，但可以读与管理只读视图。
func TestRootCapReadOnlyCannotWrite(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "auditor", Cap: "r"})

	writes := []string{
		"put -- 只读凭证不该写进来",
		"write blackegg/me/01-shape.md -- 覆盖",
		"commit blackegg/me@zero",
		"delete blackegg/me@zero/01-shape.md",
	}
	for _, line := range writes {
		if _, err := callMCP(t, srv, tok, line); err == nil {
			t.Errorf("只读凭证不该通过: %s", line)
		}
	}

	// 读路径必须照常可用，否则「只读」变成了「报废」。
	if _, err := callMCP(t, srv, tok, "read blackegg/me/01-shape.md"); err != nil {
		t.Fatalf("只读凭证应能读: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "branch blackegg/me"); err != nil {
		t.Fatalf("只读凭证应能看分支列表: %v", err)
	}
}

// 没有全局写权限的 root 凭证不能用无参 commit 一次性提交所有项目。
func TestRootCommitAllRequiresFullNamespace(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "scoped",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero"}},
	})
	if _, err := callMCP(t, srv, tok, "commit"); err == nil {
		t.Fatal("受限挂载凭证的全局 commit 应被拒绝")
	}
}

// status 按可见范围计数；未鉴权的 health 不吐规模。
func TestStatusCountsAreScoped(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	full := Caller{User: "zero", Root: true, Cap: "rwx"}

	scopedTok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "scoped",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero"}},
	})
	scoped, err := srv.authenticate(reqWith(scopedTok))
	if err != nil {
		t.Fatal(err)
	}

	fullRes, err := srv.execTerm("status", Viewer{}, full)
	if err != nil {
		t.Fatal(err)
	}
	scopedRes, err := srv.execTerm("status", Viewer{}, scoped)
	if err != nil {
		t.Fatal(err)
	}
	if fullRes.Text == scopedRes.Text {
		t.Fatalf("受限身份的切片计数应与全局不同: %q", scopedRes.Text)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	srv.muxFor(ListenBlock{Addr: "127.0.0.1:0", Allow: []string{"git-upload-pack"}}).ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, leak := range []string{"total_blocks", "docs_indexed", "terms_indexed"} {
		if strings.Contains(body, leak) {
			t.Fatalf("未鉴权 health 不该泄漏 %s: %s", leak, body)
		}
	}
	if !strings.Contains(body, "\"ok\":true") {
		t.Fatalf("health 仍应回答存活: %s", body)
	}
}

// Git Smart HTTP 必须按分支 ACL 过滤广告的 ref：
// 项目整体可见不等于每条分支可见。
func TestGitHTTPRefusesWhenSomeBranchUnreadable(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwxrwxrwx"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, br := range []string{"zero", "secret"} {
		if err := repo.Write(br, br+".md", []byte(br+" 内容\n"), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Commit(br, "init "+br); err != nil {
			t.Fatal(err)
		}
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "agent",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "daily", At: "/", Branch: "zero"}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/daily.git/info/refs?service=git-upload-pack", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+tok)
	srv.muxFor(ListenBlock{Addr: "127.0.0.1:0", Allow: []string{"git-upload-pack"}}).ServeHTTP(rec, req)

	// 协议按整仓广告 ref，无法只广告一部分而保证不泄漏；
	// 含不可读分支时唯一的诚实答案是拒绝。
	if rec.Code != http.StatusForbidden {
		t.Fatalf("仓库含不可读分支时 clone 广告应被拒绝，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 凭证能读的所有分支，广告里必须都出现（拒绝不是默认答案）。
func TestGitHTTPAdvertisesWhenAllBranchesReadable(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "all",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*"}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/blackegg/me.git/info/refs?service=git-upload-pack", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+tok)
	srv.muxFor(ListenBlock{Addr: "127.0.0.1:0", Allow: []string{"git-upload-pack"}}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("全分支可读时应正常广告，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "refs/heads/zero") {
		t.Fatalf("广告应包含可读分支: %q", rec.Body.String())
	}
}

// tag 列表按项目可见性过滤：受限凭证不该看到表外项目的 tag。
func TestTagListingRespectsMount(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	for _, p := range []string{"blackegg/me", "other/secret"} {
		repo, err := srv.projects.Get(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Tag("v1", "zero"); err != nil {
			t.Fatal(err)
		}
	}
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "narrow",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero"}},
	})
	out, err := callMCP(t, srv, tok, "tag")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "other/secret") {
		t.Fatalf("tag 列表不该出现挂载表外项目: %s", out)
	}
	if !strings.Contains(out, "blackegg/me") {
		t.Fatalf("tag 列表应包含挂载表内项目: %s", out)
	}
}

// REST /api?action=read 曾经把调用者硬编码成 root，等于把挂载表整个旁路。
func TestAPIReadUsesRealCaller(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "narrow",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero"}},
	})
	h := srv.muxFor(ListenBlock{Addr: "127.0.0.1:0"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api?action=read&addr=other/secret/x.md", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "机密") {
		t.Fatalf("API 读不得绕过挂载表: %s", rec.Body.String())
	}
}

// grep / log / ls 读的是正文，判据必须与 read 一致（不能只看挂载表）。
func TestContentVerbsHonourReadPermission(t *testing.T) {
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
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[access]]
user    = "chat"
project = "daily"
perm    = "w"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "secret.md", []byte("口令是 998877\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// chat 只有 w，没有 r：它能写，但一个字都不该读到。
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "chat", Name: "bot", Cap: "rw"})
	for _, line := range []string{"grep 998877", "log 5", "ls", "recall 998877"} {
		out, err := callMCP(t, srv, tok, line)
		if err != nil {
			continue
		}
		if strings.Contains(out, "998877") {
			t.Errorf("%q 泄漏了无 r 权限的内容: %s", line, out)
		}
	}
}

// 角色切换决定 root：只有名为 zero 的角色保留 root。
// 这条规则不能用「能力位是否齐全」代替——chat 为了拿 x 也会声明 rwx。
func TestRoleNameDecidesRootNotCapability(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	// 同一张凭证里：zero 是全量角色，chat 是受限角色。
	tokWithZero := mintRestricted(t, dataDir, auth.Claims{
		Sub:     "zero",
		Name:    "switch",
		Default: "zero",
		Roles: map[string]auth.Role{
			"zero": {Cap: "rwx", Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*"}}},
			"chat": {Cap: "rwx", Mount: []auth.MountPoint{{Project: "other/secret", At: "/", Branch: "*"}}},
		},
	})

	otherReq := func(tok, role string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":{"cmd":"ls"}}}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer "+tok)
		if role != "" {
			req.Header.Set("X-Zero-Role", role)
		}
		return req
	}

	// zero 角色：Root 成立，管理动词可用。
	cZero, err := srv.authenticate(otherReq(tokWithZero, "zero"))
	if err != nil {
		t.Fatal(err)
	}
	if !cZero.Root {
		t.Fatal("zero 角色应保留 root")
	}
	if _, err := srv.execTerm("branch blackegg/me", Viewer{}, cZero); err != nil {
		t.Fatalf("zero 角色应能管理自己挂载的仓库: %v", err)
	}

	// chat 角色：即使 cap=rwx，也不得继承 root。
	cChat, err := srv.authenticate(otherReq(tokWithZero, "chat"))
	if err != nil {
		t.Fatal(err)
	}
	if cChat.Root {
		t.Fatal("chat 角色不该保留 root（能力位齐全不等于管理身份）")
	}
	if _, err := srv.execTerm("branch other/secret", Viewer{}, cChat); err == nil {
		t.Fatal("非 root 角色不该通过管理动词")
	}
}

// 远端同步是「记忆离开本机」的动作：只读凭证不该能取回，
// 只写凭证不该能推出去，分支级 ACL 也必须生效。
func TestRemoteOpsRequireMatchingCapability(t *testing.T) {
	dataDir := t.TempDir()
	remoteDir := filepath.Join(dataDir, "remote.git")
	if err := osWrite(filepath.Join(dataDir, "keep"), ""); err != nil {
		t.Fatal(err)
	}
	runGit(t, t.TempDir(), "init", "--bare", remoteDir)

	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwxrwxrwx"

[[remote]]
project = "daily"
name    = "origin"
url     = "__REMOTE__"
push    = ["zero"]
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	cfg = strings.ReplaceAll(cfg, "__REMOTE__", remoteDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "a.md", []byte("内容\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// 只有 w 的凭证：能推，不能 fetch（fetch 会把远端内容搬进本地）。
	writerTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "writer", Cap: "w"})
	if _, err := callMCP(t, srv, writerTok, "fetch daily"); err == nil {
		t.Error("没有 r 权限的凭证不该能 fetch")
	}

	// 只有 r 的凭证：不能 push。
	readerTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "reader", Cap: "r"})
	if _, err := callMCP(t, srv, readerTok, "push daily zero"); err == nil {
		t.Error("没有 w 权限的凭证不该能 push")
	}

	// 完整凭证：sync 只报告自己能读的分支。
	mountTok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "scoped",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "daily", At: "/", Branch: "zero"}},
	})
	out, err := callMCP(t, srv, mountTok, "sync daily")
	if err != nil {
		t.Fatalf("sync 失败: %v", err)
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("sync 不该列出不可读分支: %s", out)
	}
}

// 删 tag 与归档/重置都是「让历史里少掉东西」的动作：
// 只读 root 凭证不该能做，受限挂载凭证不该越界做。
func TestDestructiveAdminVerbsNeedWrite(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Tag("v1", "zero"); err != nil {
		t.Fatal(err)
	}

	readOnly := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "ro", Cap: "r"})
	if _, err := callMCP(t, srv, readOnly, "tag -d blackegg/me v1"); err == nil {
		t.Error("只读凭证不该能删 tag")
	}

	scoped := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "scoped",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero"}},
	})
	// reset 的源在表外：读都不允许，更不该借它重置表内分支。
	if _, err := callMCP(t, srv, scoped, "reset blackegg/me@zero from other/secret@zero"); err == nil {
		t.Error("reset 不该跨到挂载表外的分支")
	}
	// archive 目标分支不在挂载表内：显式命名时拒绝。
	if _, err := callMCP(t, srv, scoped, "archive blackegg/me@zero -> blackegg/me@hidden"); err == nil {
		t.Error("归档到表外分支应被拒绝")
	}

	// 表内、有余量的凭证仍然可以删自己的 tag。
	full := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})
	if _, err := callMCP(t, srv, full, "tag -d blackegg/me v1"); err != nil {
		t.Fatalf("全权限凭证应能删 tag: %v", err)
	}
}

// 被挂载限制的 root 凭证不能碰全局管理资源。
//
// 真实事故：一张只挂 daily@zero 的凭证执行 remote-secret list 成功，
// 列出了全部凭证名。原因是 requireRootCap 只查 Root + cap——
// 而 Root 是「用户是 root」的属性，不是「这张纸没被限制」的属性。
func TestScopedRootTokenCannotTouchGlobalAdmin(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "scoped",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/"}},
	})

	for _, line := range []string{
		"remote-secret list",
		"remote-key list",
		"users",
		"config",
		"reload",
	} {
		if out, err := callMCP(t, srv, tok, line); err == nil {
			t.Errorf("%q 不该对受限凭证开放，却返回了: %s", line, out)
		}
	}

	// 而项目内的管理动词仍然按挂载放行。
	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/scoped.md -- 正文"); err != nil {
		t.Fatalf("挂载内的 write 应仍然可用: %v", err)
	}
}

// 删除一个不存在的路径必须报错，不能回「已标记删除」。
// 拼错一个字符就得到成功，是删除类操作最坏的失败方式。
func TestDeleteMissingPathErrors(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})
	if _, err := callMCP(t, srv, tok, "delete blackegg/me@zero/no-such-file.md"); err == nil {
		t.Fatal("删除不存在的文件应报错")
	}
	// 存在时仍然可以删。
	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/gone.md -- 内容"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "delete blackegg/me@zero/gone.md"); err != nil {
		t.Fatalf("存在的文件应可删: %v", err)
	}
}
