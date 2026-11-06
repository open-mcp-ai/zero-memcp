package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

func TestTermcpVarsAndArgsExpansion(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

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
	defer srv.Close()

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "tester", Cap: "rwx"})

	// 1. vars 正文里包含换行和特殊字符：不应打乱 cmd
	longBody := "第一行\n-- 第二行带横线\n第三行"
	callJSON := map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "put -- $BODY",
			"vars": map[string]string{"BODY": longBody},
		},
	}
	out, err := callMCPRaw(t, srv, tok, callJSON)
	if err != nil {
		t.Fatalf("put vars 失败: %v", err)
	}
	if !strings.Contains(out, "ok ") {
		t.Fatalf("put 应返回 ok，实际: %s", out)
	}

	// 2. args 位置参数
	callArgsJSON := map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "put -- $1",
			"args": []string{"来自位置参数的正文"},
		},
	}
	outArgs, err := callMCPRaw(t, srv, tok, callArgsJSON)
	if err != nil {
		t.Fatalf("put args 失败: %v", err)
	}
	if !strings.Contains(outArgs, "ok ") {
		t.Fatalf("put args 应返回 ok，实际: %s", outArgs)
	}

	// 3. 传了多余的 vars 必须报错（丢参数被严禁）
	callUnusedJSON := map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "status",
			"vars": map[string]string{"UNUSED": "val"},
		},
	}
	if _, err := callMCPRaw(t, srv, tok, callUnusedJSON); err == nil {
		t.Fatal("未引用的变量应直接报错")
	}

	// 4. 写了 $ 却没在 vars/args 声明必须报错
	callUndeclaredJSON := map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd": "put -- $MISSING",
		},
	}
	if _, err := callMCPRaw(t, srv, tok, callUndeclaredJSON); err == nil {
		t.Fatal("未声明的变量应直接报错")
	}
}

func callMCPRaw(t *testing.T, srv *Server, tok string, callBody map[string]any) (string, error) {
	t.Helper()
	p, err := json.Marshal(callBody)
	if err != nil {
		t.Fatal(err)
	}
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(p) + `}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(reqBody))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	srv.muxFor(ListenBlock{Addr: "127.0.0.1:0", Allow: []string{"*"}}).ServeHTTP(w, req)
	resp := w.Result()
	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("解码响应失败: %v", err)
	}
	if rpcResp.Error != nil {
		return "", errors.New(rpcResp.Error.Message)
	}
	if rpcResp.Result.IsError {
		msg := "isError=true"
		if len(rpcResp.Result.Content) > 0 {
			msg = rpcResp.Result.Content[0].Text
		}
		return "", errors.New(msg)
	}
	if len(rpcResp.Result.Content) > 0 {
		return rpcResp.Result.Content[0].Text, nil
	}
	return "", nil
}

// 正文里的标记不能被还原成解码器自己的前缀（防伪造）。
func TestPayloadMarkerCannotBeForged(t *testing.T) {
	got := expandRoundTrip(t, "正文里带 __ZMEM_ARG_AAAA__ 这种形状")
	if got != "正文里带 __ZMEM_ARG_AAAA__ 这种形状" {
		t.Fatalf("正文被解码器改写: %q", got)
	}
}

func expandRoundTrip(t *testing.T, body string) string {
	t.Helper()
	expanded, err := expandArgs("put -- $TEXT", map[string]string{"TEXT": body}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(expanded, "--")
	return restorePayload(strings.TrimSpace(rest))
}

func TestSinceEmptyBaselineListsWholeTree(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

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
	defer srv.Close()

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "first", Cap: "rwx"})
	// 提交一次，让分支头前进；since 需要一个 commit 作基线。
	if _, err := callMCP(t, srv, tok, "put team/chat -- 首次同步"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit daily@zero"); err != nil {
		t.Fatal(err)
	}

	out, err := callMCP(t, srv, tok, "since daily@zero/team/chat empty")
	if err != nil {
		t.Fatalf("empty 基线应可用: %v", err)
	}
	if !strings.Contains(out, "added") {
		t.Fatalf("首次同步应列出全部为新增: %s", out)
	}
}
