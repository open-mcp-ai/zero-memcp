package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// postTerm 把一条 DSL 行原样打到指定 handler 上，返回响应体。
func postTerm(t *testing.T, h http.Handler, token, cmd string, vars map[string]string) string {
	t.Helper()
	args := map[string]any{"cmd": cmd}
	if vars != nil {
		args["vars"] = vars
	}
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "term", "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

// issue #6：`| head` / `| tail` / `| less` 要能用，且不支持时给出的报错
// 必须是 zero-memcp 自己的规则，不能引用不存在的 `read | write`。
//
// 早先 `grep <regex> | head -40` 被 termcp 的管道检查拒掉，报错说
// 「右边必须是 `read … | write …`，这是本语言唯一的管道」——而 zero-memcp
// 里根本没有 `read | write` 这个构造。调用方被指向一条死路：它会以为
// 「换个右边就能用」，实际是这条语法在 zero-memcp 里就不存在。
func TestIssue6LightweightOutputPipes(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	const project, branch = "blackegg/me", "zero"

	// 造 12 行内容，足以观察 head/tail 的截断与顺序。
	var body strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&body, "line-%02d\n", i)
	}
	if _, e := rawTerm(t, srv, tok, "write "+project+"@"+branch+"/many.md $B",
		map[string]string{"B": body.String()}); e != "" {
		t.Fatalf("准备写入失败: %s", e)
	}

	// ① head N：只留前 N 行，并明确标出被截断。
	out, e := rawTerm(t, srv, tok, "read "+project+"@"+branch+":many.md | head 3", nil)
	if e != "" {
		t.Fatalf("`| head 3` 必须能执行: %s", e)
	}
	if !strings.Contains(out, "line-01") || strings.Contains(out, "line-04") {
		t.Fatalf("head 3 应只留前 3 行，实际:\n%s", out)
	}
	if !strings.Contains(out, "showing first 3") {
		t.Fatalf("截断必须出声，不能静默丢内容:\n%s", out)
	}

	// ② tail -2：只留最后 2 行。
	out, e = rawTerm(t, srv, tok, "read "+project+"@"+branch+":many.md | tail -2", nil)
	if e != "" {
		t.Fatalf("`| tail -2` 必须能执行: %s", e)
	}
	if !strings.Contains(out, "line-12") || strings.Contains(out, "line-10") {
		t.Fatalf("tail -2 应只留最后 2 行，实际:\n%s", out)
	}
	if !strings.Contains(out, "showing last 2") {
		t.Fatalf("tail 截断必须出声:\n%s", out)
	}

	// ③ less：无交互终端下返回完整内容，不报错也不截断。
	out, e = rawTerm(t, srv, tok, "read "+project+"@"+branch+":many.md | less", nil)
	if e != "" {
		t.Fatalf("`| less` 必须能执行: %s", e)
	}
	if !strings.Contains(out, "line-12") || strings.Contains(out, "showing") {
		t.Fatalf("less 应返回完整内容:\n%s", out)
	}

	// ④ 管道可以与语句链共存，且 `||` 的分层回落仍然成立。
	out, e = rawTerm(t, srv, tok,
		"read "+project+"@"+branch+":nope.md || read "+project+"@"+branch+":many.md | head 2", nil)
	if e != "" {
		t.Fatalf("链式 + 管道必须能执行: %s", e)
	}
	if !strings.Contains(out, "line-01") {
		t.Fatalf("|| 回落后的结果应被 head 处理:\n%s", out)
	}

	// ⑤ 不支持的管道：报错必须说 zero-memcp 的规则，不得提 `read | write`。
	_, e = rawTerm(t, srv, tok, "read "+project+"@"+branch+":many.md | sort", nil)
	if e == "" {
		t.Fatal("不支持的管道应当被拒")
	}
	if strings.Contains(e, "read … | write") || strings.Contains(e, "read ... | write") {
		t.Fatalf("报错不得引用 zero-memcp 里不存在的 read|write 语法糖: %s", e)
	}
	if !strings.Contains(e, "head") || !strings.Contains(e, "tail") || !strings.Contains(e, "less") {
		t.Fatalf("报错应说清真正支持什么，实际: %s", e)
	}

	// ⑥ 未命中时不做截断：不能把「没有命中」这条信息截掉或加工。
	out, _ = rawTerm(t, srv, tok, "grep zzq-no-such-token-zzq | head 1", nil)
	if !strings.Contains(out, "no matches") {
		t.Fatalf("未命中的正文不该被管道加工，实际: %q", out)
	}

	// ⑦ 端口策略不能被管道绕过。
	//
	// handleMCP 在执行前用 termcp.Parse 做端口白名单预检；带 `| head` 时
	// termcp 不认识 `|` 会解析失败并跳过策略（plan=nil），而随后那条写命令
	// 真会被执行——一条写命令从只读端口的检查里漏过去。修法是预检前先剥管道。
	roBlk := ListenBlock{Addr: "127.0.0.1:0", Allow: []string{"read", "grep", "recall", "ls", "log", "help", "status", "trees", "since"}}
	roSrv := httptest.NewServer(srv.muxFor(roBlk))
	defer roSrv.Close()

	// 直连写命令：应被端口策略拒绝。
	if body := postTerm(t, roSrv.Config.Handler, tok, "put "+project+"@zero $B", map[string]string{"B": "x"}); !strings.Contains(body, "not enabled on this port") {
		t.Fatalf("只读端口必须拒绝写命令，实际: %s", body)
	}
	// 加一个管道：必须同样被拒绝（这就是被堵上的绕过）。
	if body := postTerm(t, roSrv.Config.Handler, tok, "put "+project+"@zero $B | head 1", map[string]string{"B": "x"}); !strings.Contains(body, "not enabled on this port") {
		t.Fatalf("带管道的写命令不能绕过只读端口策略，实际: %s", body)
	}
}
