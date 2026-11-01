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

// 测试：树变更只通知锚定该子树的身份；未锚定或锚定其他树的身份不收。
func TestTreeWatchSSEFiltering(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

[[user]]
name = "zero"
root = true

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwx------"
`
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
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

	repoDir := filepath.Join(dataDir, "repos", "blackegg", "me.git")
	_ = os.MkdirAll(repoDir, 0o755)
	r, err := gitstore.Init(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Write("main", "init.md", []byte("init"), gitstore.ModeContent)
	_, _ = r.Commit("main", "init")

	tokTeam := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "agent-team", Cap: "rwx"})
	tokOther := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "agent-other", Cap: "rwx"})

	// agent-team 锚定在 ctx/team
	if _, err := callMCP(t, srv, tokTeam, "remind blackegg/me@main/ctx/team"); err != nil {
		t.Fatalf("remind 失败: %v", err)
	}

	callerTeam, _ := srv.authenticate(reqWith(tokTeam))
	callerOther, _ := srv.authenticate(reqWith(tokOther))

	evTeam := TreeEvent{
		Project: "blackegg/me",
		Branch:  "main",
		Commit:  "1234567890123456789012345678901234567890",
		Tree:    "0987654321098765432109876543210987654321",
		Paths:   []string{"ctx/team/2026-10-31/01.md"},
	}
	data, _ := json.Marshal(evTeam)
	frameTeam := "event: tree\ndata: " + string(data) + "\n\n"

	if !srv.eventAllowed(frameTeam, callerTeam) {
		t.Fatal("锚定在 ctx/team 的身份应该收到该事件")
	}
	if srv.eventAllowed(frameTeam, callerOther) {
		t.Fatal("未锚定的身份不该收到该事件")
	}

	// 发生在 ctx/other 的变更，两者都不该收
	evOther := TreeEvent{
		Project: "blackegg/me",
		Branch:  "main",
		Commit:  evTeam.Commit,
		Tree:    evTeam.Tree,
		Paths:   []string{"ctx/other/foo.md"},
	}
	dataOther, _ := json.Marshal(evOther)
	frameOther := "event: tree\ndata: " + string(dataOther) + "\n\n"
	if srv.eventAllowed(frameOther, callerTeam) {
		t.Fatal("虽然锚定了但路径不匹配，不该收到该事件")
	}
}

// 测试：Termcp vars / args 正文分离
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

// 中间件闭环：remind → put → tree 事件携带 previous/commit → since 取差量 → read 内容。
func TestTreeMiddlewareClosedLoop(t *testing.T) {
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

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "watcher", Cap: "rwx"})
	// 先建立干净基线。首个 commit 没有 parent，无法计算 path diff；
	// 这里测试的是「已有树上的增量提交」通知。
	if _, err := callMCP(t, srv, tok, "put seed -- seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit daily@zero"); err != nil {
		t.Fatal(err)
	}
	caller, err := srv.authenticate(reqWith(tok))
	if err != nil {
		t.Fatal(err)
	}

	// 订阅 SSE 帧，模拟客户端在线收脉冲。
	ch, cancel := srv.bcast.subscribe()
	defer cancel()

	// 1. 锚定子树
	if _, err := callMCP(t, srv, tok, "remind daily@zero/team/chat"); err != nil {
		t.Fatalf("remind 失败: %v", err)
	}

	// 2. 写入该子树
	out, err := callMCP(t, srv, tok, "put team/chat -- 闭环验证")
	if err != nil {
		t.Fatalf("put 失败: %v", err)
	}
	if !strings.Contains(out, "ok daily/team/chat/") {
		t.Fatalf("写入路径异常: %s", out)
	}

	// 2.5 折叠脏层：tree 事件由提交推进触发，不是由写入触发。
	if _, err := callMCP(t, srv, tok, "commit daily@zero"); err != nil {
		t.Fatalf("commit 失败: %v", err)
	}

	// 3. 必须收到 tree 事件，且带 previous 与 commit
	var frame []byte
	deadline := time.After(2 * time.Second)
	for {
		select {
		case frame = <-ch:
			if strings.HasPrefix(string(frame), "event: tree\n") {
				goto gotTreeEvent
			}
		case <-deadline:
			t.Fatal("锚定后写入应收到 tree 事件")
		}
	}
gotTreeEvent:
	text := string(frame)
	if !strings.HasPrefix(text, "event: tree\n") {
		t.Fatalf("事件类型应为 tree: %q", text)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(text, "event: tree\ndata: "), "\n\n")
	var ev TreeEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("解析事件失败: %v (%s)", err, payload)
	}
	if ev.Commit == "" || ev.Tree == "" {
		t.Fatalf("事件必须带 commit 与 tree: %+v", ev)
	}
	if !srv.treeWatch.Matches(caller.Identity, ev) {
		t.Fatalf("锚定者应匹配该事件: %+v", ev)
	}
	if len(ev.Paths) == 0 || !strings.HasPrefix(ev.Paths[0], "team/chat/") {
		t.Fatalf("事件应携带实际变化路径: %+v", ev.Paths)
	}

	// 4. 用事件里的 previous 取差量
	start := ev.Previous
	if start == "" {
		start = "empty"
	}
	sinceOut, err := callMCP(t, srv, tok, "since daily@zero/team/chat "+start)
	if err != nil {
		t.Fatalf("since 失败: %v", err)
	}
	if !strings.Contains(sinceOut, "added") {
		t.Fatalf("差量应包含新增切片: %s", sinceOut)
	}

	// 5. 差量里的路径能直接 read 出正文
	readOut, err := callMCP(t, srv, tok, "read "+ev.Paths[0])
	if err != nil {
		t.Fatalf("read 差量路径失败: %v", err)
	}
	if !strings.Contains(readOut, "闭环验证") {
		t.Fatalf("读回内容不含正文: %s", readOut)
	}

	// 6. 同一棵树的重复取差量为空（幂等）
	again, err := callMCP(t, srv, tok, "since daily@zero/team/chat "+ev.Commit)
	if err != nil {
		t.Fatalf("since 失败: %v", err)
	}
	if !strings.Contains(again, "没有变化") {
		t.Fatalf("同一 commit 应无变化: %s", again)
	}
}

// 未锚定的身份不该收到别人的树事件（服务端按锚定关系过滤）。
func TestTreeEventIsolationBetweenIdentities(t *testing.T) {
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

	tokA := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "a", Cap: "rwx"})
	tokB := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "b", Cap: "rwx"})
	callerA, _ := srv.authenticate(reqWith(tokA))
	callerB, _ := srv.authenticate(reqWith(tokB))

	if _, err := callMCP(t, srv, tokA, "remind daily@zero/team/chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tokB, "remind daily@zero/other"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tokA, "put team/chat -- A 的写入"); err != nil {
		t.Fatal(err)
	}

	ev := TreeEvent{Project: "daily", Branch: "zero", Paths: []string{"team/chat/2026-10-31/x.md"}}
	if !srv.treeWatch.Matches(callerA.Identity, ev) {
		t.Fatal("锚定 team/chat 的身份应匹配")
	}
	if srv.treeWatch.Matches(callerB.Identity, ev) {
		t.Fatal("锚定 other 的身份不该匹配")
	}
}

// 首次同步：基线用 empty 时，应把整棵子树当作新增返回。
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
