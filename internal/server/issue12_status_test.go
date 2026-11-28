package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// issue #12：「查不到」与「查不了」必须在协议层分开。
//
// 早先同一件事在四个动词上是三种形状，且只有 read 那一种在协议层可判：
//
//	read  不存在的地址 → -32603 内部错误（与「无权」「写错」「无此动词」同码）
//	grep  无命中       → 成功，正文 "no matches"
//	recall 无命中      → 成功，正文 "no matches"
//	ls    空日期       → 成功，正文 "no matching entries"，连 404 都不是
//
// 结果是调用方只能对正文做字符串比对——而排版会变，比对它的代码不会被告知。
//
// 现在的契约（四动词同一口径）：
//
//	「没有」= 成功 + _meta.memcp/found=false + memcp/reason=not_found
//	「查不了」= 错误 + code（-32003 forbidden / -32602 invalid_argument /
//	            -32001 unavailable / -32004 not_found）
//
// 两条都必须在协议层可判，且不依赖任何正文措辞。
func TestIssue12MissAndDenialAreMachineReadable(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	root := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 先写一条真实内容，让「有」与「没有」形成对照。
	putRes, err := callMCP(t, srv, root, "put blackegg/me@zero -- 一条存在的记忆")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	existAddr := strings.TrimPrefix(strings.Fields(putRes)[1], "blackegg/me/")
	existAddr = "blackegg/me@zero:" + existAddr
	if _, err := callMCP(t, srv, root, "commit blackegg/me@zero"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// ── ①「没有」在四个动词上是同一种形状 ──
	//
	// 必须都能判、都不断言正文。注意这四条各自命中的是不同代码路径：
	// read 走地址解析后的未命中、grep/recall 走索引未命中、ls 走日期筛选未命中。
	for _, cmd := range []string{
		"read blackegg/me@zero:1999-01-01/000000.000000-0-nobody.md",
		"grep zzq-no-such-token-zzq",
		"recall zzq-no-such-token-zzq",
		"ls 1999-01-01",
	} {
		mustMiss(t, srv, root, cmd)
	}

	// 对照组：真查到时必须报 found=true，否则「未命中」这个信号没有意义
	// （一个永远为 false 的字段与没有字段一样没用）。
	_, meta, err := callMCPMeta(t, srv, root, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if meta["memcp/found"] != true {
		t.Fatalf("有内容时 memcp/found 应为 true，实际 %v（meta=%v）", meta["memcp/found"], meta)
	}

	// ── ②「查不了」是错误，且原因进协议 ──
	//
	// 无权读：必须报 forbidden（-32003），不能降级成「没查到」——
	// 否则一次越权读取与一次空结果长得一模一样。
	outTok := mintRestricted(t, dataDir, auth.Claims{Sub: "outsider", Cap: "r",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero", Cap: "r"}}})
	code, reason, msg := mcpErrorOf(t, srv, outTok, "read other/secret@zero:anything.md")
	if code != -32003 || reason != string(RefusalForbidden) {
		t.Fatalf("越界读取应报 forbidden(-32003)，实际 code=%d reason=%q msg=%q", code, reason, msg)
	}

	// 语法错：必须是 invalid_argument（-32602，JSON-RPC 给参数错的标准码），
	// 不是 internal。
	code, reason, _ = mcpErrorOf(t, srv, root, "read")
	if code != -32602 || reason != string(RefusalInvalidArgument) {
		t.Fatalf("read 缺参数应报 invalid_argument(-32602)，实际 code=%d reason=%q", code, reason)
	}
	code, reason, _ = mcpErrorOf(t, srv, root, "nosuchverb")
	if code != -32602 || reason != string(RefusalInvalidArgument) {
		t.Fatalf("未知动词应报 invalid_argument(-32602)，实际 code=%d reason=%q", code, reason)
	}
	// 四种失败不能再共用同一个码：这条断言就是 issue 的核心。
	if code == -32603 {
		t.Fatal("失败仍然落在 -32603：调用方无法区分种类")
	}

	// ── ③ `||` 对四个动词一致生效 ──
	//
	// issue #12 实测：`grep <无命中> || read <地址>` 会回落，而
	// `read <不存在> || read <存在>` 不会——而 read 恰恰最需要分层回落
	// （先读 fact，没有就读 record）。
	out, err := callMCP(t, srv, root, "read blackegg/me@zero:1999-01-01/nope.md || read "+existAddr)
	if err != nil {
		t.Fatalf("read 未命中后 || 应回落到下一条，实际报错: %v", err)
	}
	if !strings.Contains(out, "存在的记忆") {
		t.Fatalf("|| 没有回落到存在的那条，实际: %q", out)
	}
}

// mcpErrorOf 发一条命令并取出 JSON-RPC 错误的 (code, data.reason, message)。
func mcpErrorOf(t *testing.T, srv *Server, token, cmd string) (int, string, string) {
	t.Helper()
	cmd, _, args := legacyToNative(cmd)
	argJSON, _ := json.Marshal(args)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem","arguments":{"cmd":` +
		strconv.Quote(cmd) + `,"args":` + string(argJSON) + `}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)

	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Reason string `json:"reason"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v, body: %s", err, rec.Body.String())
	}
	if resp.Error == nil {
		t.Fatalf("%s 应当被拒绝，实际没有 error 字段：%s", cmd, rec.Body.String())
	}
	return resp.Error.Code, resp.Error.Data.Reason, resp.Error.Message
}
