package server

import (
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/term"
)

// 入口规整：把调用方的值与语法分开，剩下的都交给 termcp。
//
// # 这里只做两件事
//
//  1. 校验引用：行内出现的每个 $REF 必须在 vars/args 里有值。未声明的引用
//     静默当字面量处理，会让调用方把拼错的变量名写进记忆库，而它以为写的
//     是值。termcp 自己不管这件事（缺值时报 "vars does not define"），但
//     「行内引用必须独成词、且必须已声明」这一条在这里补齐。
//  2. 补 target 哨兵：见 noTargetSentinel。
//
// # 这里不再做什么
//
// 早先这里有四段「旧写法 → termcp 形状」的转换（`-- 内联正文`、`@<sess>`
// 位置参数、`diff a -> b`、`remote-key put <n> -- $KEY`）。适配 termcp 之后
// 对外的写法本身就是它认得的形态，这些转换全部删除：
//
//	put [<tree>] [--session S] $TEXT   正文只能是一个引用（PayloadVar 强制）
//	ref <tree> <addr>                  引用写入是独立动词，不再借 put --ref
//	diff/cp/mv a --to b                `->` 在 shell 语法里是重定向符
//	remote-key put --name <n> $KEY     位置参数与引用不能同 payload

// noTargetSentinel 是「这个 payload 动词没有写 target」的占位符。
//
// termcp 的 KindPayload 要求 payload 引用之前必须有一个显式 target：
// `put $1` 会被它拒绝（"takes a VARIABLE, not a command line"），因为
// 那个引用被当成 target 消费掉了，payload 变空。
//
// 这是上游 issue #296（TargetOptional 与 payload 组合时单个引用被当作
// target）。上游修好之前用哨兵补位：协议看到一个合法的带 target 语句，
// reconstructCmd 在派发回动词之前把它剥掉。
//
// 注意：带了 --session 之类的 flag 时不需要哨兵——那属于「以 flag 开头」，
// termcp 的 TargetOptional 正好覆盖这种形态。
const noTargetSentinel = "~NOMEMTREE~"

// payloadVerbs 是 payload 动词：它们的正文只能是一个引用。
var payloadVerbs = map[string]bool{
	"put": true, "write": true,
	"remote-key": true, "remote-secret": true,
}

// normalizeTermInput 把一行的 vars/args 装配成 term.Subst，并做上面两件事。
func normalizeTermInput(cmd string, vars map[string]string, args []string) (string, term.Subst, error) {
	sub := term.Subst{
		Named: make(map[string]string, len(vars)),
		Pos:   append([]string(nil), args...),
	}
	for k, v := range vars {
		sub.Named[k] = v
	}

	if err := validateReferences(cmd, vars, args); err != nil {
		return "", term.Subst{}, err
	}

	return injectMissingTarget(cmd), sub, nil
}

// validateReferences 检查 payload 动词的行内引用都在 vars/args 里有值。
//
// # 为什么只在 payload 动词上生效
//
// termcp 的检查是有条件的：表为空时 `$X` 当字面量留给 shell 展开。这有它
// 的道理——出厂带 `run` 动词，`run echo $HOME` 里的 `$HOME` 必须到 shell 去。
//
// 但 `PayloadType: PayloadVar` 的动词不同：它已经声明「这个 payload 必须恰好
// 是一个引用」。在这样一个区域里，一个没有值的 `$X` 不可能是留给 shell 的，
// 因为 shell 根本不读这块区域——它只能是调用方拼错的名字，而后果是这个
// 字面量被当成正文存进记忆库。
//
// 反过来说，全局严格是错的：`grep $HOME` 是「搜索字面量 $HOME」，`recall
// $PRICE` 是「搜价格符号」，把它们拒掉等于削掉一个真实能力。这类动词的
// `$` 归 termcp 的既有规则管（想写字面量就用引号包住）。
func validateReferences(cmd string, vars map[string]string, args []string) error {
	for _, raw := range splitStatements(cmd) {
		fields := strings.Fields(raw)
		// 非 payload 动词：交给 termcp 的条件检查，这里不越界。
		if len(fields) == 0 || !payloadVerbs[fields[0]] {
			continue
		}
		for _, tok := range fields {
			if !strings.HasPrefix(tok, "$") || len(tok) <= 1 {
				continue
			}
			ref := tok[1:]
			// 位置参数 $1、$2 …
			if ref[0] >= '0' && ref[0] <= '9' {
				n := 0
				for _, c := range ref {
					if c < '0' || c > '9' {
						return fmt.Errorf("bad positional reference %q", tok)
					}
					n = n*10 + int(c-'0')
				}
				if n < 1 || n > len(args) {
					return fmt.Errorf("positional argument %s is not declared (have %d args)", tok, len(args))
				}
				continue
			}
			// 命名变量 $NAME
			if _, ok := vars[ref]; !ok {
				return fmt.Errorf("variable %s is not declared in vars", tok)
			}
		}
	}
	return nil
}

// injectMissingTarget 为「payload 动词 + 只有一个引用 + 没有 target」的行补哨兵。
//
// 只处理裸写形态 `put $TEXT`、`put $1`。带 flag 的形态（`put --session S $TEXT`）
// termcp 自己能处理，不要动它——给 flag 形态补哨兵会变成
// `put ~NOMEMTREE~ --session S $TEXT`，那是两个位置词，反而更糟。
func injectMissingTarget(cmd string) string {
	var out []string
	for _, raw := range splitStatements(cmd) {
		fields := strings.Fields(raw)
		if len(fields) == 2 && payloadVerbs[fields[0]] && strings.HasPrefix(fields[1], "$") {
			out = append(out, fields[0]+" "+noTargetSentinel+" "+fields[1])
			continue
		}
		out = append(out, raw)
	}
	return joinWithOperators(cmd, out)
}

// splitStatements 按 `;` / 换行 切出各分句，但保留 `&&` / `||` 在原文里。
//
// 它只服务于上面两个函数（校验与哨兵补位），不参与执行——执行的切分
// 由 termcp 的 Parse 负责，那份才是唯一的真相。这里的关系必须保持
// 「粗粒度、宁可少切」：多切一次会让一个合法语句被拆开，而漏切只是
// 少做一次校验。
func splitStatements(cmd string) []string {
	var out []string
	var cur strings.Builder
	inOffset := 0
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == ';' || c == '\n' {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
			inOffset = i + 1
			continue
		}
		cur.WriteByte(c)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	_ = inOffset
	return out
}

// joinWithOperators 把规整后的分句按原来的 `;` / `&&` / `||` 拼回一行。
//
// 传进来的 rewritten 与 cmd 的分句一一对应（数量相同），所以操作符可以
// 从原文里逐段抄回来——把 `||` 拼成 `;` 会直接抹掉门控语义。
func joinWithOperators(cmd string, rewritten []string) string {
	// 原文里每个分句之前的分隔符
	ops := statementOperators(cmd)
	var sb strings.Builder
	for i, part := range rewritten {
		if i > 0 {
			op := "; "
			if i-1 < len(ops) && ops[i-1] != "" {
				op = " " + ops[i-1] + " "
			}
			sb.WriteString(op)
		}
		sb.WriteString(strings.TrimSpace(part))
	}
	return sb.String()
}

// statementOperators 返回每个分句（第 2 个起）之前的分隔符：";"、"&&"、"||"。
func statementOperators(cmd string) []string {
	var ops []string
	for i := 0; i < len(cmd); i++ {
		switch {
		case cmd[i] == '&' && i+1 < len(cmd) && cmd[i+1] == '&':
			ops = append(ops, "&&")
			i++
		case cmd[i] == '|' && i+1 < len(cmd) && cmd[i+1] == '|':
			ops = append(ops, "||")
			i++
		case cmd[i] == ';' || cmd[i] == '\n':
			ops = append(ops, ";")
		}
	}
	return ops
}
