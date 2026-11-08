package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/term"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// zeroGrammar 声明 zero-memcp 的完整 TERM/1 动词表。
//
// 协议层由 open-mcp-ai/termcp/term 提供：
//   - 语法解析（; / && / || / &）
//   - 参数与 payload 边界防护（PayloadVar 强制变量引用，杜绝注入）
//   - 短路执行（208 已报告标记）
//   - Slot 对齐与标准 HTTP 风格信封
var zeroGrammar = term.NewGrammar([]term.Op{
	// ── 核心记忆存取 ──
	// put: 写入记忆。必须通过变量引用传递正文，正文不在 DSL 语法行里，
	// 杜绝分号/换行被误切为命令。支持通过 --ref 挂载已有切片引用。
	// put：正文必须是一个变量引用（PayloadVar 由协议强制）。
	//
	// 引用写入拆成独立的 ref 动词：PayloadVar 要求 payload 必须存在，
	// 而引用写入没有正文，一个 Op 装不下两种形态。
	{
		Name:           "put",
		Kind:           term.KindPayload,
		Target:         true,
		TargetOptional: true,
		PayloadType:    term.PayloadVar,
		Flags:          []string{"session"},
		Usage:          "put [<tree>] [--session <id>] $TEXT",
	},
	{
		Name:   "ref",
		Kind:   term.KindFlags,
		Target: true,
		Flags:  []string{"session"},
		Usage:  "ref <tree> <addr>",
	},
	{
		Name:  "recall",
		Kind:  term.KindFlags,
		Flags: []string{"scope", "bare", "all"},
		Usage: "recall <query> [--scope <tree>] [--bare]",
	},
	{
		Name:  "srecall",
		Kind:  term.KindFlags,
		Flags: []string{"scope"},
		Usage: "srecall <query> [--scope <tree>]",
	},
	{
		Name:  "read",
		Kind:  term.KindFlags,
		Usage: "read <addr> [<addr>...]",
	},
	{
		Name:  "grep",
		Kind:  term.KindFlags,
		Flags: []string{"scope", "all", "bare"},
		Usage: "grep <pattern> [--scope <tree>]",
	},
	{
		Name:  "log",
		Kind:  term.KindFlags,
		Usage: "log [N]",
	},
	{
		Name:  "ls",
		Kind:  term.KindFlags,
		Flags: []string{"all", "scope"},
		Usage: "ls [date] [--scope <tree>]",
	},
	{
		Name:  "trees",
		Kind:  term.KindFlags,
		Usage: "trees <project>[@<branch>]",
	},
	{
		Name:  "remind",
		Kind:  term.KindFlags,
		Usage: "remind [<tree>]",
	},
	{
		Name:  "since",
		Kind:  term.KindFlags,
		Usage: "since <tree> <commit|empty>",
	},
	{
		Name:  "status",
		Kind:  term.KindFlags,
		Usage: "status",
	},

	// ── 维护与分支操作 ──
	{
		Name:  "commit",
		Kind:  term.KindFlags,
		Flags: []string{"m"},
		Usage: "commit [<project>[@<branch>]] [-m msg]",
	},
	{
		Name:  "diff",
		Kind:  term.KindFlags,
		Flags: []string{"raw", "to"},
		Usage: "diff <p>@<br> [-> <p>@<br>]",
	},
	{
		Name:  "branch",
		Kind:  term.KindFlags,
		Usage: "branch [<project> [<new> [from <src>]]] ",
	},
	{
		Name:      "tag",
		Kind:      term.KindFlags,
		Flags:     []string{"d"},
		Valueless: []string{"d"},
		Usage:     "tag [-d] <project> [<name> [<ref>]]",
	},
	{
		Name:  "cp",
		Kind:  term.KindFlags,
		Flags: []string{"to"},
		Usage: "cp <src> -> <dst>",
	},
	{
		Name:  "mv",
		Kind:  term.KindFlags,
		Flags: []string{"to"},
		Usage: "mv <src> -> <dst>",
	},
	{
		Name:  "delete",
		Kind:  term.KindFlags,
		Usage: "delete <p>@<br>/<path>...",
	},
	{
		Name:  "deleted",
		Kind:  term.KindFlags,
		Usage: "deleted <project>",
	},
	{
		Name:  "archive",
		Kind:  term.KindFlags,
		Usage: "archive <p>@<br>",
	},
	{
		Name:  "reset",
		Kind:  term.KindFlags,
		Usage: "reset <p>@<br> from <src>",
	},
	{
		Name:  "lock",
		Kind:  term.KindFlags,
		Usage: "lock <p>@<br>",
	},
	{
		Name:  "unlock",
		Kind:  term.KindFlags,
		Usage: "unlock <p>@<br>",
	},
	{
		Name:  "chmod",
		Kind:  term.KindFlags,
		Usage: "chmod <p>@<br>/<path> <mode>",
	},
	{
		Name:        "write",
		Kind:        term.KindPayload,
		Target:      true,
		PayloadType: term.PayloadVar,
		Usage:       "write <p>@<br>/<path> $TEXT",
	},

	// ── 同步与远端 ──
	{
		Name:  "remote",
		Kind:  term.KindFlags,
		Usage: "remote [<project>]",
	},
	// remote-key / remote-secret 是复合动词：子命令 list/delete 不带正文，
	// 只有 put 带，且 put 还需要一个位置参数 <name>。
	//
	// termcp 的 payload 规则不允许「位置参数 + 引用」混在 payload 里
	// （引用要么是 payload 的全部，要么一个都没有），所以 <name> 必须
	// 走 flag。对外仍保留 `put <name> -- $KEY` 的写法，由
	// normalizeTermInput 双向转换。
	{
		Name:           "remote-key",
		Kind:           term.KindPayload,
		Target:         true,
		TargetOptional: true,
		PayloadType:    term.PayloadVar,
		Flags:          []string{"name"},
		Usage:          "remote-key list | put --name <name> $KEY | delete <name>",
	},
	{
		Name:           "remote-secret",
		Kind:           term.KindPayload,
		Target:         true,
		TargetOptional: true,
		PayloadType:    term.PayloadVar,
		Flags:          []string{"name"},
		Usage:          "remote-secret list | put --name <name> $TOKEN | delete <name>",
	},
	{
		Name:  "fetch",
		Kind:  term.KindFlags,
		Usage: "fetch <project> [<remote>]",
	},
	{
		Name:  "push",
		Kind:  term.KindFlags,
		Usage: "push <project> [<remote>] <branch>",
	},
	{
		Name:  "sync",
		Kind:  term.KindFlags,
		Usage: "sync <project> [<remote>]",
	},
	{
		Name:  "conflict",
		Kind:  term.KindFlags,
		Usage: "conflict [list|clear <id>]",
	},
	{
		Name:  "merge",
		Kind:  term.KindFlags,
		Usage: "merge <project> <branch> from <remote>",
	},
	{
		Name:  "merged",
		Kind:  term.KindFlags,
		Usage: "merged <project>",
	},
	{
		Name:  "ahead",
		Kind:  term.KindFlags,
		Usage: "ahead <project>",
	},
	{
		Name:  "modified",
		Kind:  term.KindFlags,
		Usage: "modified <project>",
	},
	{
		Name:   "added",
		Kind:   term.KindFlags,
		Usage:  "added <project>",
	},

	// ── 系统与权限 ──
	{
		Name:  "help",
		Kind:  term.KindFlags,
		Usage: "help [<verb>]",
	},
	{
		Name:  "whoami",
		Kind:  term.KindFlags,
		Usage: "whoami",
	},
	{
		Name:  "users",
		Kind:  term.KindFlags,
		Usage: "users",
	},
	{
		Name:  "reload",
		Kind:  term.KindFlags,
		Usage: "reload",
	},
	{
		Name:  "rule",
		Kind:  term.KindFlags,
		Usage: "rule [<project>]",
	},
	{
		Name:  "config",
		Kind:  term.KindFlags,
		Usage: "config",
	},
	{
		Name:  "content",
		Kind:  term.KindFlags,
		Usage: "content <p>@<br>/<path>",
	},
	{
		Name:  "initialize",
		Kind:  term.KindFlags,
		Usage: "initialize",
	},
})

// termHandler 实现 termcp 的 term.Handler 接口。
type termHandler struct {
	server *Server
	viewer Viewer
	caller Caller
	// lastStructured 记下最后一个动词自己的业务结构化投影
	// （status 的 terms、ls 的列表等）。
	//
	// 为什么需要它：动词产出的 Structured 是业务字段，而经过
	// envelope.Slot 之后只剩信封自身的渲染。单一 slot 的回复
	// 应该保留动词的原始投影，否则结构化消费方拿到的是空壳。
	lastStructured any
}

// Handle 将一条已解析的 Statement 派发给 zero-memcp 执行。
func (h *termHandler) Handle(ctx context.Context, st directive.Statement) []envelope.Slot {
	// 把 parsed Statement 重构为适合现有各动词内部消费的 cmd 串。
	// 这里正文用 base64 原子传递，保持内部安全。
	cmdLine := reconstructCmd(st)

	res, err := h.server.execStatement(cmdLine, h.viewer, h.caller)
	h.lastStructured = res.Structured
	if err != nil {
		return []envelope.Slot{
			{
				Code:    400,
				Headers: map[string]string{"Verb": st.Op},
				Body:    err.Error(),
			},
		}
	}

	headers := map[string]string{
		"Verb": st.Op,
	}

	// 核心：zero-memcp 的未命中回退语义。
	//
	// 当一条查询（如 recall）没有匹配项时，res.Empty 为 true。
	// 在 termcp 协议中，allOK 检查 `Code >= 300` 视为失败：
	// 因此未命中必须返回 >= 300 的状态码（404 Not Found），
	// 这样 `recall q --scope fact || recall q --scope record`
	// 的 `||` 短路才能正确被触发！
	if res.Empty {
		return []envelope.Slot{
			{
				Code:    404,
				Headers: headers,
				Body:    res.Text,
			},
		}
	}

	return []envelope.Slot{
		{
			Code:    200,
			Headers: headers,
			Body:    res.Text,
		},
	}
}

// reconstructCmd 把已解析的 Statement 还原成各动词内部认识的 rest 形式。
//
// 四个动词的内部协议与 termcp 的语法不完全一致，这里是唯一需要还原的地方：
//
//	put/ref     动词从 rest 头部读一个 `@<session>` 位置词（不是 --session）
//	put/write   动词要求正文写在 `-- <body>` 之后（不是原生 payload 位置）
//	diff/cp/mv  动词以 `->` 分隔源与目标（`->` 在 termcp 里是重定向符）
//	remote-*    动词以 `put <name>` 位置形式调用
//
// 还原的正文走 encodePayload：它是 base64 原子，不含空白与分隔符，
// 所以动词内部的 strings.Cut(rest, "--") 不会被正文里的 `--` 干扰。
func reconstructCmd(st directive.Statement) string {
	// 凭据动词：`put --name <n> $KEY` 还原成动词认识的 `put <n> -- <body>`。
	if credentialVerbs[st.Op] && st.Target == "put" {
		name := ""
		if v, ok := st.Flags["name"]; ok && v != nil {
			name = *v
		}
		if name != "" && st.Payload != "" {
			return st.Op + " put " + name + " -- " + encodePayload(st.Payload)
		}
	}

	var parts []string
	if st.Op != "" {
		parts = append(parts, st.Op)
	}
	// 哨兵剥掉：动词看到的是它原本的无 target 形式。
	if st.Target != "" && st.Target != noTargetSentinel {
		parts = append(parts, st.Target)
	}

	// diff/cp/mv 的 `--to` 还原成 `->` 分隔符。
	toVal := ""
	if v, ok := st.Flags["to"]; ok && v != nil && *v != "" {
		toVal = *v
		delete(st.Flags, "to")
	}
	// `--session` 还原成动词认识的 `@<sess>` 位置词。
	sessVal := ""
	if v, ok := st.Flags["session"]; ok && v != nil && *v != "" {
		sessVal = *v
		if !strings.HasPrefix(sessVal, "@") {
			sessVal = "@" + sessVal
		}
		delete(st.Flags, "session")
	}

	for k, v := range st.Flags {
		if v == nil || *v == "" {
			parts = append(parts, "--"+k)
		} else {
			parts = append(parts, "--"+k, *v)
		}
	}
	if sessVal != "" {
		parts = append(parts, sessVal)
	}
	for _, arg := range st.Args {
		parts = append(parts, arg)
	}
	if toVal != "" {
		parts = append(parts, "->", toVal)
	}
	if st.Payload != "" {
		parts = append(parts, "--", encodePayload(st.Payload))
	}
	return strings.Join(parts, " ")
}

// credentialVerbs 是「put 需要 <name> 位置参数 + 正文」的那两个复合动词。
var credentialVerbs = map[string]bool{"remote-key": true, "remote-secret": true}

func (s *Server) newTerm(v Viewer, c Caller) (*term.Term, *termHandler) {
	h := &termHandler{
		server: s,
		viewer: v,
		caller: c,
	}
	return term.New(zeroGrammar, term.WithHandler(h), term.WithGuide(func() envelope.Reply {
		return envelope.Reply{
			Slots: []envelope.Slot{
				{
					Code: 200,
					Body: helpOverview(),
				},
			},
		}
	})), h
}

var _ = fmt.Sprintf

// renderReply 将 termcp 的 Reply 渲染为 zero-memcp 的 TermResult 投影。
//
// 保持两种投影的承诺：
//   - Text: 标准 TERM/1 信封文本（包含状态行、头部、正文、208 已报告分枝）
//   - Structured: 结构化的 slots 数组，供程序消费
func renderReply(reply envelope.Reply, h *termHandler) TermResult {
	if len(reply.Slots) == 0 {
		return TermResult{Text: "", Empty: true}
	}

	// 只要存在一个不是 204/404 的有效 slot，就算命中有内容
	allEmpty := true
	for _, sl := range reply.Slots {
		if sl.Code == 200 {
			allEmpty = false
			break
		}
	}

	// 如果只有一个 slot，且是纯文本输出，保持旧有纯文本习惯，
	// 避免在简短回复里输出冗长的信封头。
	// 多 slot 或带短路的链式调用使用标准信封。
	if len(reply.Slots) == 1 {
		sl := reply.Slots[0]
		var st any = sl.Structured()
		if h != nil && h.lastStructured != nil {
			st = h.lastStructured
		}
		return TermResult{
			Text:       sl.Body,
			Structured: st,
			Empty:      sl.Code >= 300 || sl.Code == 204,
		}
	}

	return TermResult{
		Text:       reply.Text(),
		Structured: reply.Structured(),
		Empty:      allEmpty,
	}
}
