package server

import (
	"fmt"
	"strconv"
	"strings"
)

// pipeStage 表示语句末尾的一步轻量输出管道（issue #6）。
//
// 为什么在 zero-memcp 这一侧接管 `|`：
//
//	上游 termcp 的 `|` 专门保留给 `read <h> | write <h>`（在两个设备之间
//	搬字节），因此遇到 `grep foo | head -40` 会报错：
//	  "The right side of a termcp `|` must be one: `read … | write …`
//	   copies bytes from one node to another, and it is the only pipeline
//	   in this language."
//	在 zero-memcp 里根本没有 `read | write` 这个构造——把使用者引向一个
//	当前 DSL 不存在的语法糖，等于指了一条死路。
//
// 这里的原则：
//   - 正文永远走 vars/args（从不进 cmd 语法行），因此 `|` 不会误伤正文内容；
//   - 支持最高频的输出截断/查看：`| head [N]`、`| tail [N]`、`| less`；
//   - 不支持的管道当场用 zero-memcp 自己的规则报错，绝不透传给 termcp 吐出
//     不存在的 `read | write` 提示。
type pipeStage struct {
	op    string // "head" | "tail" | "less"
	lines int
}

type pipedStmt struct {
	left   string      // 剥掉管道后交给 termcp 解析执行的那一句
	stages []pipeStage // 从左到右依次应用
}

// splitPipelineSegments 把整行按 `;` / `\n` / `&&` / `||` 切成语句，
// 并剥出每条语句右侧的轻量管道。
//
// 返回值 hasPipe 为 false 时，整行没有任何管道，调用方走原来的快速路径
// （原封不动交给 termcp）。
func parseCommandPipes(cmd string) (stmts []pipedStmt, ops []string, hasPipe bool, err error) {
	rawStmts, rawOps := splitTopLevelStatements(cmd)
	stmts = make([]pipedStmt, 0, len(rawStmts))
	for _, raw := range rawStmts {
		segs := splitSinglePipes(raw)
		if len(segs) == 1 {
			stmts = append(stmts, pipedStmt{left: segs[0]})
			continue
		}
		hasPipe = true
		left := strings.TrimSpace(segs[0])
		if left == "" {
			return nil, nil, false, invalidf(nil, "empty command before '|'")
		}
		var stages []pipeStage
		for _, r := range segs[1:] {
			st, perr := parsePipeStage(r)
			if perr != nil {
				return nil, nil, false, perr
			}
			stages = append(stages, st)
		}
		stmts = append(stmts, pipedStmt{left: left, stages: stages})
	}
	return stmts, rawOps, hasPipe, nil
}

// stripCommandPipes 只返回「把管道剥干净后的语法行」，供端口级权限预检使用。
//
// 安全原因：handleMCP 在执行前用 termcp.Parse 做端口白名单检查；如果
// 命令行带 `| head`，termcp.Parse 会因不认识而跳过策略（plan=nil），
// 随后真执行时就会绕过只读端口的写拦截。先剥管道再预检，两处看法一致。
func stripCommandPipes(cmd string) string {
	stmts, ops, hasPipe, err := parseCommandPipes(cmd)
	if err != nil || !hasPipe {
		return cmd
	}
	parts := make([]string, len(stmts))
	for i, st := range stmts {
		parts[i] = st.left
	}
	return rebuildChain(parts, ops)
}

func rebuildChain(parts, ops []string) string {
	var sb strings.Builder
	for i, p := range parts {
		if i > 0 {
			sep := "; "
			if i-1 < len(ops) && ops[i-1] != "" {
				sep = " " + ops[i-1] + " "
			}
			sb.WriteString(sep)
		}
		sb.WriteString(strings.TrimSpace(p))
	}
	return sb.String()
}

// parsePipeStage 解析单个管道阶段：只认 head / tail / less。
// 任何其它阶段都给出明确、不含 `read | write` 误导的拒绝。
func parsePipeStage(raw string) (pipeStage, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return pipeStage{}, invalidf(nil,
			"expected a pipe stage after '|' (supported: head [N], tail [N], less)")
	}
	op := fields[0]
	rest := fields[1:]
	switch op {
	case "less":
		if len(rest) > 0 {
			return pipeStage{}, invalidf(map[string]any{"pipe": op},
				"less takes no arguments here (supported pipes: head [N], tail [N], less)")
		}
		return pipeStage{op: "less"}, nil
	case "head", "tail":
		n, err := parseLineCount(op, rest)
		if err != nil {
			return pipeStage{}, err
		}
		return pipeStage{op: op, lines: n}, nil
	default:
		// 这里绝不透传给 termcp：后者会提示 `read … | write …`，而 zero-memcp
		// 里没有这个构造。直接说出这里真正支持什么。
		return pipeStage{}, invalidf(map[string]any{"pipe": op},
			"unsupported pipe stage %q: zero-memcp supports '| head [N]', '| tail [N]', and '| less' only "+
				"(arbitrary shell pipelines and 'read | write' are not supported)", op)
	}
}

func parseLineCount(op string, args []string) (int, error) {
	const defLines = 10
	switch len(args) {
	case 0:
		return defLines, nil
	case 1:
		a := args[0]
		if strings.HasPrefix(a, "-") {
			a = a[1:]
		}
		n, err := strconv.Atoi(a)
		if err != nil || n < 0 {
			return 0, invalidf(map[string]any{"pipe": op},
				"%s: invalid line count %q (use %s [N], %s -N, or %s -n N)", op, args[0], op, op, op)
		}
		return n, nil
	case 2:
		if args[0] != "-n" {
			return 0, invalidf(map[string]any{"pipe": op},
				"%s: unsupported flag %q (use %s [N], %s -N, or %s -n N)", op, args[0], op, op, op)
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 {
			return 0, invalidf(map[string]any{"pipe": op},
				"%s: invalid line count %q", op, args[1])
		}
		return n, nil
	default:
		return 0, invalidf(map[string]any{"pipe": op},
			"usage: %s [N | -N | -n N]", op)
	}
}

// applyPipeStages 按顺序对输出文本应用 head / tail / less。
//
// 空命中（Empty == true，例如 "no matches"）不做截断：总不能把一条
// 提示没命中的消息截成空白，或者在上面加「showing first 10 of 1」。
func applyPipeStages(text string, empty bool, stages []pipeStage) string {
	if len(stages) == 0 || empty || text == "" {
		return text
	}
	cur := text
	for _, st := range stages {
		switch st.op {
		case "less":
			// 无交互终端里的 less：返回完整内容，不报错。
		case "head":
			cur = sliceHeadLines(cur, st.lines)
		case "tail":
			cur = sliceTailLines(cur, st.lines)
		}
	}
	return cur
}

func sliceHeadLines(text string, n int) string {
	if n == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[:n], "\n") +
		fmt.Sprintf("\n[head: showing first %d of %d lines]", n, len(lines))
}

func sliceTailLines(text string, n int) string {
	if n == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) <= n {
		return text
	}
	return fmt.Sprintf("[tail: showing last %d of %d lines]\n", n, len(lines)) +
		strings.Join(lines[len(lines)-n:], "\n")
}

// splitTopLevelStatements 在感知引号与转义的前提下按 `;` / `\n` / `&&` / `||`
// 切分语句。单竖线 `|` 不切（它留给语句内部的管道解析）。
func splitTopLevelStatements(cmd string) (stmts []string, ops []string) {
	var cur strings.Builder
	var quote byte
	escaped := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			cur.WriteByte(c)
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			cur.WriteByte(c)
			continue
		}
		if c == '&' && i+1 < len(cmd) && cmd[i+1] == '&' {
			if s := strings.TrimSpace(cur.String()); s != "" {
				stmts = append(stmts, s)
				ops = append(ops, "&&")
			}
			cur.Reset()
			i++
			continue
		}
		if c == '|' && i+1 < len(cmd) && cmd[i+1] == '|' {
			if s := strings.TrimSpace(cur.String()); s != "" {
				stmts = append(stmts, s)
				ops = append(ops, "||")
			}
			cur.Reset()
			i++
			continue
		}
		if c == ';' || c == '\n' {
			if s := strings.TrimSpace(cur.String()); s != "" {
				stmts = append(stmts, s)
				ops = append(ops, ";")
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	if len(ops) >= len(stmts) && len(stmts) > 0 {
		ops = ops[:len(stmts)-1]
	}
	return stmts, ops
}

// splitSinglePipes 在单条语句内按引号外的单 `|` 切分（跳过 `||`）。
func splitSinglePipes(stmt string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	escaped := false
	for i := 0; i < len(stmt); i++ {
		c := stmt[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			cur.WriteByte(c)
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			cur.WriteByte(c)
			continue
		}
		if c == '|' {
			if i+1 < len(stmt) && stmt[i+1] == '|' {
				cur.WriteString("||")
				i++
				continue
			}
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}
