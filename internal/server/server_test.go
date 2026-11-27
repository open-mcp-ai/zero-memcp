package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

func newTestServer(t *testing.T, tomlContent string) (*Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfgPath := filepath.Join(dataDir, "zero-memcp.toml")
	if tomlContent == "" {
		tomlContent = `
[server]
data = "` + filepath.ToSlash(dataDir) + `"

[[user]]
name = "zero"
root = true

[[project]]
name = "daily"
owner = "zero"
mode = "rwx------"

[[project]]
name = "me"
owner = "zero"
mode = "rwxr-xr-x"
`
	}
	if err := osWrite(cfgPath, tomlContent); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  cfgPath,
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 仓库的脏层写者是后台 goroutine：不关就会在 TempDir 清理时
	// 继续往仓库目录写文件，撞上 RemoveAll。Close 幂等，测试里
	// 显式再关一次也无害。
	t.Cleanup(func() { _ = srv.Close() })
	return srv, dataDir
}

// osWrite 写测试文件。
//
// 写 .toml 配置时统一把反斜杠转成正斜杠：
// Windows 上 t.TempDir() 返回 C:\Users\...，把这种路径直接拼进
// TOML 双引号字符串会被当成 Unicode 转义序列 \UXXXXXXXX，解析直接报
// "unsupported escape sequence \U"。TOML 与 Go 的 os 包在 Windows 上
// 都认正斜杠，所以转一下既合法又等价。
//
// 只在 .toml 上转：正文内容里的反斜杠是数据，不能动。
func osWrite(path, content string) error {
	if strings.HasSuffix(path, ".toml") {
		content = strings.ReplaceAll(content, `\`, "/")
		content = withIndex(content)
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// withIndex 给测试配置补上 [index] enabled = true。
//
// 索引现在是「配置才生效」：没有 [index] 节的实例按冷备份运行，
// 不建倒排表、不预热正文缓存，recall/grep 直接报错。
//
// 绝大多数测试要的是一个**可搜索**的节点（那才是生产形态），
// 所以在这里统一声明，而不是让每个测试各自记着写一节。
// 需要冷备份形态的测试自己构造配置、不要走这个 helper。
func withIndex(cfg string) string {
	if strings.Contains(cfg, "[index]") {
		return cfg
	}
	return cfg + "\n[index]\nenabled = true\n"
}

// waitIndexReady 等后台索引构建结束。
//
// [index] async 的默认值是 true（配置驱动的实例），所以 New() 返回时
// 树可能还没加载完——「先监听端口，再建索引」正是这个默认的全部意义。
// 测试若要断言「一启动就能查到」，就必须显式等；不等就断言，测到的是
// 竞态而不是功能。
func waitIndexReady(t *testing.T, s *Server) {
	t.Helper()
	if s == nil || s.indexBuildDone == nil {
		return
	}
	select {
	case <-s.indexBuildDone:
	case <-time.After(120 * time.Second):
		t.Fatal("后台索引构建 120s 未完成")
	}
}

func mintToken(t *testing.T, dataDir, sub string) string {
	t.Helper()
	secret, err := auth.LoadOrCreateSecret(filepath.Join(dataDir, "system", "jwt.key"))
	if err != nil {
		t.Fatal(err)
	}
	jti, err := auth.NewJTI()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.NewTokenStore(secret).Sign(auth.Claims{
		Sub: sub,
		Exp: time.Now().Add(time.Hour).Unix(),
		JTI: jti,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func callMCP(t *testing.T, srv *Server, token, cmd string) (string, error) {
	t.Helper()
	// 脚手架把旧写法翻成 termcp 原生语法（服务端只认原生）。
	cmd, _, args := legacyToNative(cmd)
	argJSON, _ := json.Marshal(args)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":{"cmd":` +
		strconv.Quote(cmd) + `,"args":` + string(argJSON) + `}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)

	// 鉴权失败时返回的是 HTTP 401 纯文本，不是 JSON-RPC 包体。
	if rec.Code == http.StatusUnauthorized {
		return "", errors.New(strings.TrimSpace(rec.Body.String()))
	}

	return mcpText(t, rec.Body.Bytes())
}

// callMCPMeta 与 callMCP 同源，但把 result._meta 一并交出来。
//
// 为什么测试需要它：issue #12 之后，「查不到」不再是错误，而是一个
// 机器可判的字段。断言「err != nil」等于在断言旧契约；断言
// _meta["memcp/found"] == false 才是在测现在真正承诺的东西。
func callMCPMeta(t *testing.T, srv *Server, token, cmd string) (text string, meta map[string]any, err error) {
	t.Helper()
	cmd, _, args := legacyToNative(cmd)
	argJSON, _ := json.Marshal(args)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":{"cmd":` +
		strconv.Quote(cmd) + `,"args":` + string(argJSON) + `}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		return "", nil, errors.New(strings.TrimSpace(rec.Body.String()))
	}
	text, err = mcpText(t, rec.Body.Bytes())
	var resp struct {
		Result struct {
			Meta map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if jerr := json.Unmarshal(rec.Body.Bytes(), &resp); jerr == nil {
		meta = resp.Result.Meta
	}
	return text, meta, err
}

// mustMiss 断言一条命令「跑成功了，但什么都没查到」。
//
// 这是 issue #12 要的那条不变式：查不到既不是错误，也不是一句
// "no matches" 的字符串约定，而是 _meta 里一个可判的字段。
func mustMiss(t *testing.T, srv *Server, token, cmd string) {
	t.Helper()
	text, meta, err := callMCPMeta(t, srv, token, cmd)
	if err != nil {
		t.Fatalf("%s 应报「未命中」而不是错误: %v", cmd, err)
	}
	found, ok := meta["memcp/found"]
	if !ok {
		t.Fatalf("%s 的结果缺少 _meta.memcp/found，调用方无从判断查没查到", cmd)
	}
	if found != false {
		t.Fatalf("%s 应报未命中，实际 memcp/found=%v（text=%q）", cmd, found, text)
	}
}

// mcpText 从 JSON-RPC 响应里取出文本投影。
func mcpText(t *testing.T, body []byte) (string, error) {
	t.Helper()
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析响应: %v, body: %s", err, string(body))
	}
	if resp.Error != nil {
		return "", errors.New(resp.Error.Message)
	}
	if len(resp.Result.Content) > 0 {
		return resp.Result.Content[0].Text, nil
	}
	return "", nil
}

// 写入一条记忆，再把它读回来，这是整套服务存在的理由。
func TestWriteReadEndToEnd(t *testing.T) {
	srv, dataDir := newTestServer(t, "")
	tok := mintToken(t, dataDir, "zero")

	res, err := callMCP(t, srv, tok, "put -- 第一条记忆")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !strings.HasPrefix(res, "ok daily/") {
		t.Fatalf("put 返回异常: %s", res)
	}
	// 契约：`ok ` 之后的第一个词就是地址，括号里是上下文（落点提示 / base）。
	addr := firstToken(strings.TrimPrefix(res, "ok "))

	readRes, err := callMCP(t, srv, tok, "read "+addr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(readRes, "第一条记忆") {
		t.Fatalf("读出的内容不对: %s", readRes)
	}
}

// 权限：项目级 mode 生效，未授权的人读不到、写不进。
func TestPermissionEnforced(t *testing.T) {
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "alice"
root = true

[[user]]
name = "bob"

[[project]]
name  = "private"
owner = "alice"
mode  = "rwx------"

[[project]]
name  = "public"
owner = "alice"
mode  = "rwxr-xr-x"
`
	dataDir := t.TempDir()
	cfg = strings.ReplaceAll(cfg, "__DATA__", filepath.ToSlash(dataDir))
	_ = osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg)

	// 在两个仓库里各放一个文件
	rPriv, _ := gitstore.Init(filepath.Join(dataDir, "repos", "private.git"))
	_ = rPriv.Write("zero", "sec.md", []byte("机密\n"), gitstore.ModeContent)
	_, _ = rPriv.Commit("zero", "c")

	rPub, _ := gitstore.Init(filepath.Join(dataDir, "repos", "public.git"))
	_ = rPub.Write("zero", "pub.md", []byte("公开\n"), gitstore.ModeContent)
	_, _ = rPub.Commit("zero", "c")

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	aliceTok := mintToken(t, dataDir, "alice")
	bobTok := mintToken(t, dataDir, "bob")

	// alice 能读机密
	if out, err := callMCP(t, srv, aliceTok, "read private/sec.md"); err != nil || !strings.Contains(out, "机密") {
		t.Fatalf("alice 应能读机密: %v, out: %s", err, out)
	}

	// bob 读机密被拒
	out, rerr := callMCP(t, srv, bobTok, "read private/sec.md")
	if strings.Contains(out, "机密") {
		t.Fatalf("bob 不该能读到机密: %s", out)
	}
	// 「看不见」必须是错误，不是「没查到」（issue #12）：
	// 后者会让一次越权读取与一次空结果长得一模一样。
	if rerr == nil {
		t.Fatal("bob 读机密应被拒绝，而不是得到空结果")
	}
	if !strings.Contains(rerr.Error(), "lacks r permission") {
		t.Fatalf("应给出权限不足的理由，实际: %v", rerr)
	}

	// bob 读公开内容应放行
	if out, err := callMCP(t, srv, bobTok, "read public/pub.md"); err != nil || !strings.Contains(out, "公开") {
		t.Fatalf("bob 应能读公开内容: %v, out: %s", err, out)
	}
}

// 规则位注入：100755 的条目每轮注入系统提示，100644 的不注入。
func TestRuleBitInjectedIntoSystem(t *testing.T) {
	dataDir := t.TempDir()
	r, err := gitstore.Init(filepath.Join(dataDir, "repos", "me.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Write("zero", "rules/01.md", []byte("我是 Zero\n"), gitstore.ModeRule)
	_ = r.Write("zero", "notes/01.md", []byte("普通的笔记\n"), gitstore.ModeContent)
	_, _ = r.Commit("zero", "init")

	srv, err := New(Config{
		DataDir:       dataDir,
		DefaultBranch: "zero",
		Listen:        "127.0.0.1:0",
		IndexEnabled:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	req := httptest.NewRequest(http.MethodGet, "/identity", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.handleIdentity(rec, req)

	var resp struct {
		Blocks []struct {
			Addr string `json:"addr"`
			Text string `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}

	// 规则块必须在
	foundRule := false
	for _, b := range resp.Blocks {
		if b.Addr == "me/rules/01.md" && strings.Contains(b.Text, "我是 Zero") {
			foundRule = true
		}
		if b.Addr == "me/notes/01.md" {
			t.Fatal("普通内容不该被注入进系统提示")
		}
	}
	if !foundRule {
		t.Fatal("规则块未被注入")
	}
}

// 吊销：吊销后同一张凭证立刻被拒绝。
func TestRevocationEnforced(t *testing.T) {
	srv, dataDir := newTestServer(t, "")
	secret, _ := auth.LoadOrCreateSecret(filepath.Join(dataDir, "system", "jwt.key"))
	jti, _ := auth.NewJTI()
	tok, _ := auth.NewTokenStore(secret).Sign(auth.Claims{
		Sub: "zero",
		Exp: time.Now().Add(time.Hour).Unix(),
		JTI: jti,
	})

	// 正常能调
	if _, err := callMCP(t, srv, tok, "status"); err != nil {
		t.Fatalf("正常凭证应可用: %v", err)
	}

	// 吊销
	rev := auth.NewRevocations(filepath.Join(dataDir, "system", "revoked"))
	if err := rev.Revoke(jti); err != nil {
		t.Fatal(err)
	}

	// 再次调用必须被拒绝
	_, err := callMCP(t, srv, tok, "status")
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("吊销后应被拒绝，实际: %v", err)
	}
}

// 自动提交：仓库级 <repo>.git/config 的 [memcp] 覆盖全局配置。
func TestCountTriggeredCommit(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + filepath.ToSlash(dataDir) + `"

[commit]
interval = "1h"    # 全局设得很长
count    = 100     # 全局条数也很高

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`
	_ = osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg)
	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	repo, err := srv.projects.Ensure("daily")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Start() }()
	defer srv.Close()
	time.Sleep(100 * time.Millisecond) // 等维护循环起来

	tok := mintToken(t, dataDir, "zero")

	// put 只进脏层，不自动推进分支。
	_, _ = callMCP(t, srv, tok, "put -- 1")
	_, _ = callMCP(t, srv, tok, "put -- 2")
	if n := repo.Pending("zero"); n != 2 {
		t.Fatalf("put 后 pending 应为 2，得到 %d", n)
	}

	// 显式 commit 之后分支才真正前进。
	sha, err := callMCP(t, srv, tok, "commit daily@zero")
	if err != nil || !strings.Contains(sha, "committed") {
		t.Fatalf("commit 失败: %v (%s)", err, sha)
	}
	if c, _ := repo.Resolve("zero"); c == "" {
		t.Fatal("commit 之后分支应已前进")
	}

	// 再写一条并 commit
	_, _ = callMCP(t, srv, tok, "put -- 3")
	_, _ = callMCP(t, srv, tok, "commit daily@zero")
	log, err := repo.Log("zero", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 {
		t.Fatalf("2 次显式 commit 应产生 2 次提交，实际 %d 次", len(log))
	}

	// 显式 commit 在没有待提交内容时不该造出空提交。
	before, _ := repo.Resolve("zero")
	if _, err := callMCP(t, srv, tok, "commit daily@zero"); err != nil {
		t.Fatalf("显式 commit 失败: %v", err)
	}
	after, _ := repo.Resolve("zero")
	if before != after {
		t.Fatal("没有待提交内容时不该产生新提交")
	}
}

// rootCaller 是测试里默认的调用者：主人。
func rootCaller() Caller { return Caller{User: "zero", Root: true} }

// 两级编辑权：root 绕过所有 mode；子用户按项目 mode 判定。
func TestRootBypassesAndSubUserRestricted(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[user]]
name = "bob"

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[project]]
name  = "shared"
owner = "zero"
mode  = "rwxrwxrwx"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", filepath.ToSlash(dataDir))
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"daily", "shared"} {
		r, err := gitstore.Init(filepath.Join(dataDir, "repos", p+".git"))
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Write("zero", "seed.md", []byte("种子\n"), gitstore.ModeContent)
		_, _ = r.Commit("zero", "init")
	}

	srv, err := New(Config{
		DataDir:       dataDir,
		ConfigFile:    filepath.Join(dataDir, "zero-memcp.toml"),
		DefaultBranch: "zero",
		Listen:        "127.0.0.1:0",
		EnforceAuth:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	rootTok := mintToken(t, dataDir, "zero")
	bobTok := mintToken(t, dataDir, "bob")

	// root：写受保护的 daily 应当成功
	if _, err := callMCP(t, srv, rootTok, "put -- root 写的"); err != nil {
		t.Fatalf("root 应能写入 daily: %v", err)
	}

	// 子用户显式指定 daily 写入：应当被拒绝，且不能悄悄改写到别的仓库
	_, err = callMCP(t, srv, bobTok, "put daily -- bob 想写的")
	if err == nil || !strings.Contains(err.Error(), "lacks w permission") {
		t.Fatalf("子用户写 daily 应被拒绝，实际: %v", err)
	}

	// 不指定目标时：自动挑一个他能写的，而不是报错
	res, err := callMCP(t, srv, bobTok, "put -- bob 能写的")
	if err != nil {
		t.Fatalf("应自动选到 shared: %v", err)
	}
	if !strings.HasPrefix(res, "ok shared/") {
		t.Fatalf("自动选择应落到 shared，实际: %s", res)
	}

	// 子用户：读 shared 应当放行
	if out, err := callMCP(t, srv, bobTok, "read shared/seed.md"); err != nil || !strings.Contains(out, "种子") {
		t.Fatalf("子用户应能读 shared: %v, out=%s", err, out)
	}
}

// firstToken 取响应里的地址：`ok ` 之后的第一个词。
//
// 契约是「第一个词是地址，其余是上下文（落点提示、base）」。早先有些
// 用例把整行都当地址用，只是因为新建仓库没有 base 而侥幸成立——
// 一旦返回体带上说明就会炸。
func firstToken(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}
