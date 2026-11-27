package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 多级权限收紧（token 定上限 → 动态 role 只能往下收）。
//
// 不变式：**role 只能让权限从 true 变成 false。**
// 下面每一条都在验这件事的不同侧面。
func setupRoleServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[index]
enabled = true

[[user]]
name = "zero"
root = true

[[project]]
name  = "BlackEggAI/zero"
owner = "zero"
mode  = "rwxrwxrwx"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", filepath.ToSlash(dataDir))
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "BlackEggAI", "zero.git"))
	if err != nil {
		t.Fatal(err)
	}
	// 两条兄弟子树：forum 与 chat，各自有独特词。
	for _, f := range []struct{ path, word string }{
		{"record/forum/1.md", "FORUMWORD"},
		{"record/chat/1.md", "CHATWORD"},
		{"record/public/1.md", "PUBLICWORD"},
	} {
		if err := repo.Write("main.record", f.path, []byte(f.word+"\n"), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Commit("main.record", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir: dataDir, ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"), Listen: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	return srv, dataDir, rootTok
}

// role 定义与绑定走 MCP，权限边界立刻生效。
func TestRoleNarrowsSubtreeAcrossAllVerbs(t *testing.T) {
	srv, dataDir, rootTok := setupRoleServer(t)

	// 子 agent 的凭证：能看整个项目（天花板很宽），但只挂 main.record。
	if err := auth.SaveTokenRecord(dataDir, auth.TokenRecord{
		JTI: "jti-bot", Name: "group-bot", Sub: "zero",
	}); err != nil {
		t.Fatal(err)
	}
	botTok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "group-bot", Cap: "rw",
		Mount: []auth.MountPoint{{Project: "BlackEggAI/zero", Branch: "main.record", At: "/", Cap: "rw"}},
	})

	// 定义 role：只允许 record/forum 子树，只读。
	if _, e := rawTerm(t, srv, rootTok,
		`role put forum-only "BlackEggAI/zero@main.record:record/forum" r`, nil); e != "" {
		t.Fatalf("role put 失败: %s", e)
	}
	// 未绑定前使用它必须被拒（拼错/未绑不能静默等于「不限制」）。
	if _, e := rawTerm(t, srv, botTok, "recall FORUMWORD", map[string]string{}, ); e == "" {
		// 无 role 时应当正常放行——这里只是确认基线可读
	}
	_, e := rawTermWithRole(t, srv, botTok, "recall FORUMWORD", "forum-only")
	if e == "" {
		t.Fatal("未绑定的 role 必须被拒绝，不能静默当「不限制」")
	}
	if !strings.Contains(e, "not bound") {
		t.Fatalf("报错要说清是未绑定: %s", e)
	}
	if _, e := rawTerm(t, srv, rootTok, "role bind forum-only group-bot", nil); e != "" {
		t.Fatalf("role bind 失败: %s", e)
	}

	// ① 子树收窄：forum 可见，chat 不可见——而且 read / grep / ls / log /
	//    recall 五条路径必须**同口径**（只拦地址解析等于 grep 全库照旧漏）。
	if out, e := rawTermWithRole(t, srv, botTok, "read record/forum/1.md", "forum-only"); e != "" || !strings.Contains(out, "FORUMWORD") {
		t.Fatalf("role 范围内的读应放行: err=%q out=%q", e, out)
	}
	if _, e := rawTermWithRole(t, srv, botTok, "read record/chat/1.md", "forum-only"); e == "" {
		t.Fatal("role 范围外的 read 必须被拒")
	}
	for _, cmd := range []string{"grep CHATWORD", "ls", "log", "recall CHATWORD"} {
		out, e := rawTermWithRole(t, srv, botTok, cmd, "forum-only")
		if e != "" {
			t.Fatalf("%s 不该报错（应当只是过滤掉）: %s", cmd, e)
		}
		if strings.Contains(out, "CHATWORD") || strings.Contains(out, "chat/1.md") {
			t.Fatalf("%s 把 role 范围外的内容漏出来了:\n%s", cmd, out)
		}
	}
	// 范围内的内容在这些动词里仍应看得见（否则「收窄」变成了「全禁」）。
	if out, _ := rawTermWithRole(t, srv, botTok, "grep FORUMWORD", "forum-only"); !strings.Contains(out, "forum/1.md") {
		t.Fatalf("role 范围内的 grep 命中应保留:\n%s", out)
	}
	if out, _ := rawTermWithRole(t, srv, botTok, "recall FORUMWORD", "forum-only"); !strings.Contains(out, "forum/1.md") {
		t.Fatalf("role 范围内的 recall 命中应保留:\n%s", out)
	}

	// ② 只读：写被拒（role 给了 r，cap 明明有 w）。
	if _, e := rawTermWithRole(t, srv, botTok, "write record/forum/2.md $B",
		"forum-only", map[string]string{"B": "x"}); e == "" {
		t.Fatal("role 只给 r 时，写必须被拒")
	}
	// put 不经过 resolveAddr，单独验一次（它是最常用的写动词）。
	if _, e := rawTermWithRole(t, srv, botTok, "put $B", "forum-only", map[string]string{"B": "x"}); e == "" {
		t.Fatal("role 只给 r 时，put 也必须被拒")
	}

	// ③ 不带 role 时不受影响：同一张凭证照样能读 chat。
	if out, e := rawTerm(t, srv, botTok, "read record/chat/1.md", nil); e != "" || !strings.Contains(out, "CHATWORD") {
		t.Fatalf("不带 role 时不应被收窄: err=%q out=%q", e, out)
	}

	// ④ 未知 role 直接拒（而不是静默不限制）。
	_, e = rawTermWithRole(t, srv, botTok, "recall FORUMWORD", "no-such-role")
	if e == "" || !strings.Contains(e, "no role named") {
		t.Fatalf("未知 role 必须明确拒绝: %q", e)
	}
}

// role 绝不能加宽：ACL 与 role 都想越权时，token 天花板是最后一道闸。
func TestRoleAndACLCannotWidenBeyondTokenCeiling(t *testing.T) {
	srv, dataDir, rootTok := setupRoleServer(t)

	if err := auth.SaveTokenRecord(dataDir, auth.TokenRecord{
		JTI: "jti-narrow", Name: "narrow-bot", Sub: "zero",
	}); err != nil {
		t.Fatal(err)
	}
	// 天花板很窄：只挂 record/forum 子树，cap 只有 r。
	narrowTok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "narrow-bot", Cap: "r",
		Mount: []auth.MountPoint{{
			Project: "BlackEggAI/zero", Branch: "main.record",
			Path: "record/forum", At: "/", Cap: "r",
		}},
	})

	// ① ACL 试图给它开整条分支的读写（rw）：仍然越不过 token 的 r + forum 天花板。
	if _, e := rawTerm(t, srv, rootTok, "acl put narrow-bot BlackEggAI/zero@main.record rw", nil); e != "" {
		t.Fatalf("acl put 失败: %s", e)
	}
	if _, e := rawTerm(t, srv, narrowTok, "write record/chat/9.md $B", map[string]string{"B": "x"}); e == "" {
		t.Fatal("ACL 不能把凭证写到挂载表外的子树（天花板在最底层拦）")
	}

	// ② role 试图给出更宽的边界（另一个子树）：定义时就不该被接受——
	//    但即使被接受，调用时也必须仍然收窄在 token 之内。
	if _, e := rawTerm(t, srv, rootTok,
		`role put wide "BlackEggAI/zero@main.record" rw`, nil); e != "" {
		t.Fatalf("role put 失败: %s", e)
	}
	if _, e := rawTerm(t, srv, rootTok, "role bind wide narrow-bot", nil); e != "" {
		t.Fatalf("role bind 失败: %s", e)
	}
	// 天花板只到 record/forum，role 写的是整条分支 → 仍然只能看 forum。
	//
	// 断言看**内容**而不是看是否报错：挂载子树的相对路径会先被前缀成
	// `record/forum/record/chat/1.md`，于是表现是「不存在」而不是
	// 「越界」——两种都不泄漏，但只有内容断言能证明这一点。
	if out, _ := rawTermWithRole(t, srv, narrowTok, "read record/chat/1.md", "wide"); strings.Contains(out, "CHATWORD") {
		t.Fatalf("role 不能越过 token 天花板（挂载表子树）:\n%s", out)
	}
	// 子树挂载是 chroot：这张凭证眼里 `record/forum` 就是根，里面只有 1.md。
	if out, e := rawTermWithRole(t, srv, narrowTok, "read 1.md", "wide"); e != "" || !strings.Contains(out, "FORUMWORD") {
		t.Fatalf("天花板内的读仍应放行: err=%q out=%q", e, out)
	}
}

// role 里没写到的位置一律拒绝：空 role 不是「不限制」。
func TestEmptyRoleDeniesEverything(t *testing.T) {
	srv, dataDir, rootTok := setupRoleServer(t)
	if err := auth.SaveTokenRecord(dataDir, auth.TokenRecord{
		JTI: "jti-e", Name: "empty-bot", Sub: "zero",
	}); err != nil {
		t.Fatal(err)
	}
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "empty-bot", Cap: "rw",
		Mount: []auth.MountPoint{{Project: "BlackEggAI/zero", Branch: "main.record", At: "/", Cap: "rw"}},
	})
	if _, e := rawTerm(t, srv, rootTok, `role put nothing "BlackEggAI/zero@main.record:record/nope" r`, nil); e != "" {
		t.Fatal(e)
	}
	if _, e := rawTerm(t, srv, rootTok, "role bind nothing empty-bot", nil); e != "" {
		t.Fatal(e)
	}
	if _, e := rawTermWithRole(t, srv, tok, "read record/forum/1.md", "nothing"); e == "" {
		t.Fatal("role 没写到的位置必须拒绝（空 role ≠ 不限制）")
	}
	// grep / recall 也要一致：不能只在 read 上拒。
	if out, _ := rawTermWithRole(t, srv, tok, "grep FORUMWORD", "nothing"); strings.Contains(out, "FORUMWORD") {
		t.Fatalf("role 未覆盖的位置不能从 grep 漏出:\n%s", out)
	}
}

// rawTermWithRole 是 rawTerm 的带 role 版本：走真实 MCP 调用路径
// （arguments.role），不经任何测试脚手架。
func rawTermWithRole(t *testing.T, srv *Server, token, cmd, role string, vars ...map[string]string) (string, string) {
	t.Helper()
	args := map[string]any{"cmd": cmd, "role": role}
	if len(vars) > 0 && vars[0] != nil {
		args["vars"] = vars[0]
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":` +
		jsonMust(t, args) + `}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v, body: %s", err, rec.Body.String())
	}
	if resp.Error != nil {
		return "", resp.Error.Message
	}
	if len(resp.Result.Content) > 0 {
		return resp.Result.Content[0].Text, ""
	}
	return "", ""
}

// vmcp 转发时往 HTTP 头加 X-Memcp-Role：与 MCP 参数 role 完全等价。
func TestRoleViaHTTPHeaderMatchesMCPField(t *testing.T) {
	srv, dataDir, rootTok := setupRoleServer(t)
	if err := auth.SaveTokenRecord(dataDir, auth.TokenRecord{
		JTI: "jti-vmcp", Name: "vmcp-bot", Sub: "zero",
	}); err != nil {
		t.Fatal(err)
	}
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "vmcp-bot", Cap: "rw",
		Mount: []auth.MountPoint{{Project: "BlackEggAI/zero", Branch: "main.record", At: "/", Cap: "rw"}},
	})
	if _, e := rawTerm(t, srv, rootTok, `role put forum-hdr "BlackEggAI/zero@main.record:record/forum" r`, nil); e != "" {
		t.Fatal(e)
	}
	if _, e := rawTerm(t, srv, rootTok, "role bind forum-hdr vmcp-bot", nil); e != "" {
		t.Fatal(e)
	}

	callHdr := func(cmd, hdrRole string) (int, string) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":{"cmd":` +
			jsonMust(t, cmd) + `}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		if hdrRole != "" {
			req.Header.Set("X-Memcp-Role", hdrRole)
		}
		rec := httptest.NewRecorder()
		srv.handleMCP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// 范围内放行
	if _, b := callHdr("read record/forum/1.md", "forum-hdr"); !strings.Contains(b, "FORUMWORD") {
		t.Fatalf("HTTP 头带 role 应放行范围内的读: %s", b)
	}
	// 范围外拦截
	if _, b := callHdr("read record/chat/1.md", "forum-hdr"); !strings.Contains(b, "outside role") {
		t.Fatalf("HTTP 头带 role 必须拦截范围外的读: %s", b)
	}
	// 扫库动词也要受同一份限制
	if _, b := callHdr("grep CHATWORD", "forum-hdr"); strings.Contains(b, "CHATWORD") {
		t.Fatalf("HTTP 头带 role 不能从 grep 漏出范围外内容: %s", b)
	}
	// 伪造未绑定 role 必须被拒
	if _, b := callHdr("read record/forum/1.md", "forged-admin"); !strings.Contains(b, "no role named") {
		t.Fatalf("伪造未知 role 必须被拒: %s", b)
	}
}

// 分支级动词（commit / branch …）只有 allowed() 那一层能拦：
// 它们不经过地址解析，也没有具体路径可判。role 必须同样生效。
func TestRoleNarrowsBranchLevelVerbs(t *testing.T) {
	srv, dataDir, rootTok := setupRoleServer(t)
	if err := auth.SaveTokenRecord(dataDir, auth.TokenRecord{
		JTI: "jti-bl", Name: "branch-bot", Sub: "zero",
	}); err != nil {
		t.Fatal(err)
	}
	// 凭证本身覆盖整条分支（没有子树挂载），收窄完全靠 role。
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "branch-bot", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "BlackEggAI/zero", Branch: "main.record", At: "/", Cap: "rwx"}},
	})
	// role 只给 record/forum 的读 → 对整条分支的写权限一律没有。
	if _, e := rawTerm(t, srv, rootTok, `role put sub-only "BlackEggAI/zero@main.record:record/forum" r`, nil); e != "" {
		t.Fatal(e)
	}
	if _, e := rawTerm(t, srv, rootTok, "role bind sub-only branch-bot", nil); e != "" {
		t.Fatal(e)
	}

	// 不带 role：能写（基线正确，否则下面那条断言会因为别的原因通过）。
	if _, e := rawTerm(t, srv, tok, "put record/forum/x.md $B", map[string]string{"B": "基线"}); e != "" {
		t.Fatalf("基线写入应成功: %s", e)
	}

	// 带 role：commit 整条分支必须被拒，而且理由必须指向 role
	// （区分「被 role 拦」与「被别的规则拦」——否则测试会在错误的原因下变绿）。
	_, e := rawTermWithRole(t, srv, tok, "commit BlackEggAI/zero@main.record", "sub-only")
	if e == "" {
		t.Fatal("role 未授予写时，分支级动词 commit 也必须被拒（只有 allowed() 这层能拦）")
	}
	if !strings.Contains(e, "role") {
		t.Fatalf("拒绝理由应指向 role，实际: %s", e)
	}
}

// put 不经过地址解析：它的落点由服务端生成，可能正好落在 role 边界之外。
// 这条只有 writeNoteGit 里那层判定能拦。
func TestRoleNarrowsPutLandingPath(t *testing.T) {
	srv, dataDir, rootTok := setupRoleServer(t)
	if err := auth.SaveTokenRecord(dataDir, auth.TokenRecord{
		JTI: "jti-put", Name: "put-bot", Sub: "zero",
	}); err != nil {
		t.Fatal(err)
	}
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "put-bot", Cap: "rw",
		Mount: []auth.MountPoint{{Project: "BlackEggAI/zero", Branch: "main.record", At: "/", Cap: "rw"}},
	})
	// role 给了写，但只在 record/forum 子树里。
	if _, e := rawTerm(t, srv, rootTok, `role put forum-rw "BlackEggAI/zero@main.record:record/forum" rw`, nil); e != "" {
		t.Fatal(e)
	}
	if _, e := rawTerm(t, srv, rootTok, "role bind forum-rw put-bot", nil); e != "" {
		t.Fatal(e)
	}
	// ① 无树前缀的 put 落在分支根（日期目录）——在 role 边界之外，必须被拒。
	if _, e := rawTermWithRole(t, srv, tok, "put $B", "forum-rw", map[string]string{"B": "越界写入"}); e == "" {
		t.Fatal("put 的落点落在 role 边界之外时必须被拒（只有写路径那层能拦）")
	}
	// ② 落在 role 边界之内的 put 应当成功。
	if _, e := rawTermWithRole(t, srv, tok, "put record/forum $B", "forum-rw", map[string]string{"B": "界内写入"}); e != "" {
		t.Fatalf("role 边界内的 put 应放行: %s", e)
	}
}
