package server

import (
	"context"
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
		Detail:         detail_put,
		Kind:           term.KindPayload,
		Target:         true,
		TargetOptional: true,
		PayloadType:    term.PayloadVar,
		Flags:          []string{"session", "where", "dry-run"},
		Usage:          "put [<tree>] [--session <id>] [--where] $TEXT",
		// Detail 是 help put 的正文。它住在动词声明里而不是 help.go：
		// 一个动词的用法和它的解释分开写，改一处忘一处是迟早的事。
	},
	{
		Name:   "ref",
		Detail: detail_ref,
		Kind:   term.KindFlags,
		Target: true,
		Flags:  []string{"session"},
		Usage:  "ref <tree> <addr>",
	},
	{
		Name:      "recall",
		Detail:    detail_recall,
		Kind:      term.KindFlags,
		// before / after / older-than 带值，所以不进 Valueless。
		// 不登记会被 term 层的「unknown flag」挡在动词之前，
		// 而 parseTimeWindow 根本轮不到执行。
		Flags:     []string{"scope", "bare", "all", "json", "before", "after", "older-than"},
		Valueless: []string{"bare", "all", "json"},
		Usage:     "recall <query> [--scope <tree>] [--before <date>] [--older-than <2h|3d>] [--bare] [--json]",
	},
	{
		Name:      "srecall",
		Kind:      term.KindFlags,
		Flags:     []string{"scope", "json"},
		Valueless: []string{"json"},
		Usage:     "srecall <query> [--scope <tree>] [--json]",
	},
	{
		Name:   "read",
		Detail: detail_read,
		Kind:   term.KindFlags,
		Usage:  "read <addr> [<addr>...]",
	},
	{
		Name:   "grep",
		Detail: detail_grep,
		Kind:   term.KindFlags,
		Flags:  []string{"scope", "all", "bare", "i", "ignore-case", "c", "count", "l", "files-only"},
		Usage:  "grep [-i] [-c | -l] <pattern> [--scope <tree>]",
	},
	{
		Name:   "log",
		Detail: detail_log,
		Kind:   term.KindFlags,
		Usage:  "log [N]",
	},
	{
		Name:   "ls",
		Detail: detail_ls,
		Kind:   term.KindFlags,
		Flags:  []string{"all", "scope"},
		Usage:  "ls [date] [--scope <tree>]",
	},
	{
		Name:   "trees",
		Detail: detail_trees,
		Kind:   term.KindFlags,
		Usage:  "trees <project>[@<branch>]",
	},
	{
		Name:   "remind",
		Detail: detail_remind,
		Kind:   term.KindFlags,
		// --reanchor：显式把基线推到当前头。
		//
		// 缺省（不带 flag）时必须幂等，否则 agent 每次启动重登 watch
		// 都会把停机期间的变化抹平（见 watch_restart_test.go）。
		Flags: []string{"reanchor"},
		Usage: "remind [<tree>] [--reanchor]",
	},
	{
		Name:   "since",
		Detail: detail_since,
		Kind:   term.KindFlags,
		Usage:  "since <tree> <commit|empty>",
	},
	{
		Name:   "status",
		Detail: detail_status,
		Kind:   term.KindFlags,
		Usage:  "status",
	},

	// ── 维护与分支操作 ──
	{
		Name:   "commit",
		Detail: detail_commit,
		Kind:   term.KindFlags,
		Flags:  []string{"m"},
		Usage:  "commit [<project>[@<branch>]] [-m msg]",
	},
	{
		// pending：列出上次 commit 之后还没进版本库的路径。
		//
		// 归纳流程的第一步。它不认识任何业务概念——不知道谁是人格层、
		// 谁是流水，只回答「哪些路径还没进 commit」。
		Name:   "pending",
		Detail: detail_pending,
		Kind:   term.KindFlags,
		Usage:  "pending <project>[@<branch>]",
	},
	{
		Name:   "diff",
		Detail: detail_diff,
		Kind:   term.KindFlags,
		Flags:  []string{"raw", "to"},
		Usage:  "diff <p>@<br> [--to <p>@<br>]",
	},
	{
		Name:   "branch",
		Detail: detail_branch,
		Kind:   term.KindFlags,
		Usage:  "branch [<project> [<new> [from <src>]]] ",
	},
	{
		Name:      "tag",
		Detail:    detail_tag,
		Kind:      term.KindFlags,
		Flags:     []string{"d", "m"},
		Valueless: []string{"d"},
		Usage:     "tag [-d] <project> [<name> [<ref>]] [-m msg]",
	},
	{
		Name:   "cp",
		Detail: detail_cp,
		Kind:   term.KindFlags,
		Flags:  []string{"to"},
		Usage:  "cp <src> --to <dst>",
	},
	{
		Name:   "mv",
		Detail: detail_cp,
		Kind:   term.KindFlags,
		Flags:  []string{"to"},
		Usage:  "mv <src> --to <dst>",
	},
	{
		Name:   "ledger",
		Detail: detail_ledger,
		Kind:   term.KindFlags,
		Flags:  []string{"result", "identity", "channel", "action", "hash", "tree", "since", "limit"},
		Usage:  "ledger check <external_id> | ledger put <external_id> --result R | ledger list",
	},
	{
		Name:   "role",
		Detail: detail_role,
		Kind:   term.KindFlags,
		Flags:  []string{"note"},
		Usage:  "role ls | put <name> <spec> <perm> [...] | bind <name> <identity>... | unbind <name> | rm <name>",
	},
	{
		Name:   "delete",
		Detail: detail_delete,
		Kind:   term.KindFlags,
		Usage:  "delete <p>@<br>/<path>...",
	},
	{
		Name:   "erase",
		Detail: detail_erase,
		Kind:   term.KindFlags,
		Target: true,
		Flags:  []string{"nonce", "sig", "expires"},
		Usage:  "erase <p>@<br>/<path> [--nonce N --expires T --sig B64]",
	},
	{
		Name:        "replace",
		Detail:      detail_replace,
		Kind:        term.KindPayload,
		Target:      true,
		Flags:       []string{"nonce", "sig", "expires"},
		PayloadType: term.PayloadVar,
		Usage:       "replace <p>@<br>/<path> [--nonce N --expires T --sig B64] $TEXT",
	},
	{
		Name:  "deleted",
		Kind:  term.KindFlags,
		Usage: "deleted <project>",
	},
	{
		Name:   "archive",
		Detail: detail_archive,
		Kind:   term.KindFlags,
		Flags:  []string{"to"},
		Usage:  "archive <p>@<br> [--to <p>@<as>]",
	},
	{
		Name:   "reset",
		Detail: detail_reset,
		Kind:   term.KindFlags,
		Usage:  "reset <p>@<br> from <src>",
	},
	{
		Name:   "lock",
		Detail: detail_lock,
		Kind:   term.KindFlags,
		Usage:  "lock <p>@<br>",
	},
	{
		Name:   "unlock",
		Detail: detail_lock,
		Kind:   term.KindFlags,
		Usage:  "unlock <p>@<br>",
	},
	{
		Name:   "chmod",
		Detail: detail_chmod,
		Kind:   term.KindFlags,
		Usage:  "chmod <p>@<br>/<path> <mode>",
	},
	{
		Name:        "write",
		Detail:      detail_write,
		Kind:        term.KindPayload,
		Target:      true,
		PayloadType: term.PayloadVar,
		Usage:       "write <p>@<br>/<path> $TEXT",
	},
	{
		Name:        "apply",
		Detail:      detail_apply,
		Kind:        term.KindPayload,
		Target:      true,
		PayloadType: term.PayloadVar,
		Usage:       "apply <project>[@<branch>] $PATCHES",
	},

	// ── 同步与远端 ──
	{
		Name:   "remote",
		Detail: detail_remote,
		Kind:   term.KindFlags,
		Usage:  "remote [<project>]",
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
		Detail:         detail_remote_key,
		Kind:           term.KindPayload,
		Target:         true,
		TargetOptional: true,
		PayloadType:    term.PayloadVar,
		Flags:          []string{"name"},
		Usage:          "remote-key list | put --name <name> $KEY | delete <name>",
	},
	{
		Name:           "remote-secret",
		Detail:         detail_remote_secret,
		Kind:           term.KindPayload,
		Target:         true,
		TargetOptional: true,
		PayloadType:    term.PayloadVar,
		Flags:          []string{"name"},
		Usage:          "remote-secret list | put --name <name> $TOKEN | delete <name>",
	},
	{
		Name:   "fetch",
		Detail: detail_remote,
		Kind:   term.KindFlags,
		Usage:  "fetch <project> [<remote>]",
	},
	{
		Name:   "push",
		Detail: detail_remote,
		Kind:   term.KindFlags,
		Flags:  []string{"snapshot", "force"},
		Usage:  "push <project> [<remote>] <branch> [--snapshot] [--force]",
	},
	{
		Name:   "sync",
		Detail: detail_remote,
		Kind:   term.KindFlags,
		// 两种形态：无参 = 本节点的上下游拓扑；带项目 = 对某个远端跑一次同步。
		Usage: "sync [<project> [<remote>]]",
	},
	{
		Name:  "conflict",
		Kind:  term.KindFlags,
		Usage: "conflict [list|clear <id>]",
	},
	{
		Name:   "merge",
		Detail: detail_remote,
		Kind:   term.KindFlags,
		Usage:  "merge <project> <branch> from <remote>",
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
		Name:  "added",
		Kind:  term.KindFlags,
		Usage: "added <project>",
	},

	// ── 系统与权限 ──
	{
		Name:   "help",
		Detail: detail_help,
		Kind:   term.KindFlags,
		Usage:  "help [<verb>]",
	},
	{
		Name:   "whoami",
		Detail: detail_whoami,
		Kind:   term.KindFlags,
		Usage:  "whoami",
	},
	{
		Name:   "users",
		Detail: detail_users,
		Kind:   term.KindFlags,
		Usage:  "users",
	},
	{
		Name:   "reload",
		Detail: detail_reload,
		Kind:   term.KindFlags,
		Usage:  "reload",
	},
	{
		Name:  "rule",
		Kind:  term.KindFlags,
		Usage: "rule [<project>]",
	},
	{
		Name:   "acl",
		Detail: detail_acl,
		Kind:   term.KindFlags,
		Usage:  "acl list | acl put <user> <project>[@<branch>] <perm> | acl rm <user> <target>",
	},
	{
		Name:   "config",
		Detail: detail_config,
		Kind:   term.KindFlags,
		Flags:  []string{"include", "exclude", "reset"},
		Usage:  "config show | path | put <TOML> | index [--include A,B | --exclude A,B | --reset]",
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
	// lastRefusal / lastEmptyReason 让「拒绝的种类」与「未命中的原因」
	// 穿过 termcp 的信封层。
	//
	// 信封只有 Code + Body，类型在这里会丢：不记的话，MCP 层拿到的
	// 永远是 errors.New(body)，于是 issue #12 抱怨的「所有失败一个码」
	// 又回来了——只是这次是被协议层抹平的。
	lastRefusal     *Refusal
	lastEmptyReason string

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
	h.lastEmptyReason = res.EmptyReason
	h.lastRefusal = asRefusal(err)
	if err != nil {
		// 状态行必须与协议标志一致（issue #12 第 3 点）。
		//
		// 早先所有失败都是 400，于是「无权」「不存在」「参数错」在语句链里
		// 长得一模一样，而单语句又把状态行剥掉——调用方看到的只剩正文。
		// 现在按拒绝种类给码：403 无权、404 不存在、400 参数错、503 不可达。
		code := 400
		if ref := asRefusal(err); ref != nil {
			code = slotCodeOf(ref.Kind)
		}
		return []envelope.Slot{
			{
				Code:    code,
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

// emptyReason 给出「这次为什么没查到」，缺省按 not_found。
func (h *termHandler) emptyReason() string {
	if h != nil && h.lastEmptyReason != "" {
		return h.lastEmptyReason
	}
	return string(RefusalNotFound)
}

// slotCodeOf 把拒绝种类映射成 TERM/1 的状态码。
//
// 只用四个码，与 HTTP 的既有含义对齐：
//
//	403 forbidden / 404 not_found / 400 invalid_argument / 503 unavailable
func slotCodeOf(kind RefusalKind) int {
	switch kind {
	case RefusalForbidden:
		return 403
	case RefusalNotFound:
		return 404
	case RefusalUnavailable:
		return 503
	default:
		return 400
	}
}

// reconstructCmd 把已解析的 Statement 还原成各动词内部认识的 rest 形式。
//
// 四个动词的内部协议与 termcp 的语法不完全一致，这里是唯一需要还原的地方：
//
//	put/ref     动词从 rest 头部读一个 `@<session>` 位置词（不是 --session）
//	put/write   动词要求正文写在 `-- <body>` 之后（不是原生 payload 位置）
//	diff/cp/mv/archive  动词以 `->` 分隔源与目标（`->` 在 termcp 里是重定向符，
//	            所以对外统一写 `--to`；调用方写 `->` 也会在入口处归一，见
//	            term_compat.go 的 normalizeArrowSeparator）
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
const detail_pending = `pending —— 列出尚未提交的改动路径。

用法：pending <project>[@<branch>]

输出每条改动一行，前面是 +（新增或覆盖）或 -（删除），
后面是可直接喂给 read 的规范地址（project@branch:path）。

commit 之后清单清空。

它只回答「哪些路径还没进 commit」。至于这批改动该被如何归纳、
哪些值得留下、commit message 写什么，都是调用方的事。`

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
		out := TermResult{
			Text:       sl.Body,
			Structured: st,
			Empty:      sl.Code >= 300 || sl.Code == 204,
		}
		if out.Empty {
			out.EmptyReason = h.emptyReason()
		}
		return out
	}

	out := TermResult{
		Text:       reply.Text(),
		Structured: reply.Structured(),
		Empty:      allEmpty,
	}
	if out.Empty {
		out.EmptyReason = h.emptyReason()
	}
	return out
}

const detail_apply = `apply —— 批量写（一次工具往返改写/删除多个文件）。

用法：
  apply <project>[@<branch>] $PATCHES

其中 $PATCHES 必须是 JSON 数组（通过 vars/args 传递）：
  [
    {"op": "write",  "path": "SKILL.md",    "content": "..."},
    {"op": "write",  "path": "07-topic.md", "content": "..."},
    {"op": "delete", "path": "08-old.md"}
  ]

为什么用它而不是连续多次 write：
  1. 一次往返：不用在模型上下文里塞 N 条工具调用与返回
  2. 先全部校验、再全部落盘：语法/路径/权限有一处错，一个字节都不写
  3. 逐个 path 刷新索引，不重扫整条分支

注意：apply 只写脏层、不自动 commit。写与封箱是两件事，
全部写完由调用方显式调用 commit。`
