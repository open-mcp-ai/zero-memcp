package server

import (
	"encoding/json"
	"errors"
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

func newTestServer(t *testing.T, tomlContent string) (*Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfgPath := filepath.Join(dataDir, "zero-memcp.toml")
	if tomlContent == "" {
		tomlContent = `
[server]
data = "` + dataDir + `"

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

func osWrite(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
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
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":{"cmd":"` + cmd + `"}}}`
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
	addr := strings.TrimPrefix(res, "ok ")

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
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
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

	aliceTok := mintToken(t, dataDir, "alice")
	bobTok := mintToken(t, dataDir, "bob")

	// alice 能读机密
	if out, err := callMCP(t, srv, aliceTok, "read private/sec.md"); err != nil || !strings.Contains(out, "机密") {
		t.Fatalf("alice 应能读机密: %v, out: %s", err, out)
	}

	// bob 读机密被拒
	out, _ := callMCP(t, srv, bobTok, "read private/sec.md")
	if strings.Contains(out, "机密") {
		t.Fatalf("bob 不该能读到机密: %s", out)
	}
	if !strings.Contains(out, "缺少 r 权限") {
		t.Fatalf("应给出权限不足的理由，实际: %s", out)
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
		DataDir:        dataDir,
		DefaultProfile: "zero",
		Listen:         "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}

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
	if err == nil || !strings.Contains(err.Error(), "吊销") {
		t.Fatalf("吊销后应被拒绝，实际: %v", err)
	}
}

// 自动提交：仓库级 <repo>.git/config 的 [memcp] 覆盖全局配置。
func TestCountTriggeredCommit(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

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
	if err != nil || !strings.Contains(sha, "已提交") {
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
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
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
		DataDir:        dataDir,
		ConfigFile:     filepath.Join(dataDir, "zero-memcp.toml"),
		DefaultProfile: "zero",
		Listen:         "127.0.0.1:0",
		EnforceAuth:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	rootTok := mintToken(t, dataDir, "zero")
	bobTok := mintToken(t, dataDir, "bob")

	// root：写受保护的 daily 应当成功
	if _, err := callMCP(t, srv, rootTok, "put -- root 写的"); err != nil {
		t.Fatalf("root 应能写入 daily: %v", err)
	}

	// 子用户显式指定 daily 写入：应当被拒绝，且不能悄悄改写到别的仓库
	_, err = callMCP(t, srv, bobTok, "put daily -- bob 想写的")
	if err == nil || !strings.Contains(err.Error(), "缺少 w 权限") {
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
