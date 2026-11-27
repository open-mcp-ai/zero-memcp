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

// 直接打 MCP 端点，不经 legacyToNative 脚手架。
//
// 这两条 issue（#7 / #8）的共同形状是：**测试脚手架替调用方做了转换，
// 于是文档教了一个服务端不认的写法，而测试一直绿**。
// `legacyToNative` 会把 `-- $BODY` 剥成 `$BODY`、把 ` -> ` 换成 ` --to `，
// 正好把要测的东西抹掉。所以这里的用例必须走真实调用路径。
func rawTerm(t *testing.T, srv *Server, token, cmd string, vars map[string]string) (text string, rpcErr string) {
	t.Helper()
	args := map[string]any{"cmd": cmd}
	if vars != nil {
		args["vars"] = vars
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

func jsonMust(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// issue #8：help 里写的 `write <path> -- $BODY` 必须真能跑。
//
// 早先帮助页同时给出两种写法，其中带 `--` 的两种都会报
// `unknown flag for write: --`——文档教了一个自己拒掉的写法，
// 而同一个页面的 Example 行又是对的（一页两种答案）。
//
// 规矩没变：正文只能是一个变量引用（正文永不参与解析）。
// 变的只是「前面多写了一个 `--`」不再导致失败。
func TestIssue8WriteAcceptsDocumentedDashForm(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	const project, branch = "blackegg/me", "zero"

	// ① 文档形式（带 `--`）：必须成功。
	out, rpcErr := rawTerm(t, srv, tok, "write "+project+"@"+branch+"/doc-a.md -- $BODY",
		map[string]string{"BODY": "- 列表项\n---\n"})
	if rpcErr != "" {
		t.Fatalf("help 教的 `-- $BODY` 写法不能失败: %s", rpcErr)
	}
	if !strings.Contains(out, "written to") {
		t.Fatalf("应报告写入: %q", out)
	}

	// ② 原生形式（不带 `--`）：同样必须成功，且内容一致。
	if _, rpcErr := rawTerm(t, srv, tok, "write "+project+"@"+branch+"/doc-b.md $BODY",
		map[string]string{"BODY": "- 列表项\n---\n"}); rpcErr != "" {
		t.Fatalf("原生 $BODY 写法不能失败: %s", rpcErr)
	}
	a, e1 := rawTerm(t, srv, tok, "read "+project+"@"+branch+":doc-a.md", nil)
	b, e2 := rawTerm(t, srv, tok, "read "+project+"@"+branch+":doc-b.md", nil)
	if e1 != "" || e2 != "" {
		t.Fatalf("读回失败: %s / %s", e1, e2)
	}
	if a != b {
		t.Fatalf("两种写法写出的内容必须一致:\nA=%q\nB=%q", a, b)
	}
	// 以 `-` 开头的正文（Markdown 列表、frontmatter 分隔线）是 issue 点名的
	// 高危形状：必须原样落盘，不被当成 flag。
	if !strings.HasPrefix(strings.TrimPrefix(a, "\n"), "- 列表项") {
		t.Fatalf("以 - 开头的正文必须原样落盘，实际: %q", a)
	}

	// ③ 内联正文仍然必须被拒——这条规矩不能被上面的兼容性放松。
	if _, rpcErr = rawTerm(t, srv, tok, "write "+project+"@"+branch+"/doc-c.md -- hello", nil); rpcErr == "" {
		t.Fatal("内联正文必须被拒（正文永不参与解析）")
	}
}

// issue #7：help 教的 `->` 与语法层唯一认的 `--to` 必须都能执行。
//
// 早先 `help mv` / `help cp` / `help diff` / `help archive` 全教 `->`，
// 而 DSL 把 `>` 当重定向拒掉，只有 `--to` 能过；报错正文此刻又在 usage
// 行里继续教 `->`——三个说法里两个不可执行，按文档写命令 100% 失败。
func TestIssue7ArrowAndToFlagBothWork(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	const project = "blackegg/me"

	if _, e := rawTerm(t, srv, tok, "write "+project+"@zero/src.md $B", map[string]string{"B": "内容"}); e != "" {
		t.Fatalf("准备写入失败: %s", e)
	}
	if _, e := rawTerm(t, srv, tok, "branch "+project+" feature", nil); e != "" {
		t.Fatalf("建分支失败: %s", e)
	}

	// ① 文档形式 `->`
	if out, e := rawTerm(t, srv, tok, "cp "+project+"@zero/src.md -> "+project+"@feature/copied.md", nil); e != "" {
		t.Fatalf("`->` 写法必须能执行（help 与工具描述都这么教）: %s", e)
	} else if !strings.Contains(out, "copied.md") {
		t.Fatalf("cp 回显异常: %q", out)
	}
	// ② 语法形式 `--to`
	if _, e := rawTerm(t, srv, tok, "cp "+project+"@zero/src.md --to "+project+"@feature/copied2.md", nil); e != "" {
		t.Fatalf("`--to` 写法必须能执行: %s", e)
	}
	for _, p := range []string{"copied.md", "copied2.md"} {
		if _, e := rawTerm(t, srv, tok, "read "+project+"@feature:"+p, nil); e != "" {
			t.Fatalf("%s 应已落在 feature 分支: %s", p, e)
		}
	}

	// ③ mv 同理
	if _, e := rawTerm(t, srv, tok, "mv "+project+"@zero/src.md -> "+project+"@feature/moved.md", nil); e != "" {
		t.Fatalf("mv 的 `->` 写法必须能执行: %s", e)
	}

	// ④ diff 的 `->` 与 `--to` 都应能出结果
	if out, e := rawTerm(t, srv, tok, "diff "+project+"@zero -> "+project+"@feature", nil); e != "" {
		t.Fatalf("diff 的 `->` 写法必须能执行: %s", e)
	} else if !strings.Contains(out, "change") {
		t.Fatalf("diff 回显异常: %q", out)
	}
	if _, e := rawTerm(t, srv, tok, "diff "+project+"@zero --to "+project+"@feature", nil); e != "" {
		t.Fatalf("diff 的 `--to` 写法必须能执行: %s", e)
	}

	// ⑤ 报错里的 usage 不得再教一个被自己拒掉的写法：
	//    这里让 archive 的目标非法，检查回显中不再出现 `->`。
	_, e := rawTerm(t, srv, tok, "archive "+project+"@feature --to "+project+"@feature", nil)
	if e == "" {
		t.Fatal("同源归档应该被拒")
	}
	if strings.Contains(e, "->") {
		t.Fatalf("报错不该再教 `->`（它曾经只在报错里活着）: %s", e)
	}
	_ = strconv.Itoa
}
