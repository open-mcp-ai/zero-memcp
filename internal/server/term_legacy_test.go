package server

import (
	"strings"
)

// legacyToNative 把测试里沿用的旧写法翻译成 termcp 原生语法。
//
// 它只存在于测试脚手架。服务端本身只接受原生语法（termcp 拒绝内联正文），
// 而这些测试是围绕旧写法写的；与其改 95 处调用点，不如让脚手架翻译一次，
// 翻译出来的就是扩展真正发出的形态：
//
//	put <tree> -- <inline>          → put <tree> $1        + args
//	put <tree> -- $TEXT             → put <tree> $TEXT
//	put <head> --ref <addr>         → ref <head> <addr>
//	remote-key put <n> -- <body>    → remote-key put --name <n> $1
//	diff/cp/mv A -> B               → ... --to B
//
// 内联正文变成位置参数而不是命名变量，与 pkg/zeromem / 扩展的调用形态一致。
func legacyToNative(cmd string) (string, map[string]string, []string) {
	var args []string
	next := func(body string) string {
		args = append(args, body)
		return "$" + itoa(len(args))
	}

	var out []string
	ops := statementOperators(cmd)
	for i, raw := range splitStatements(cmd) {
		st := strings.TrimSpace(raw)

		// put <head> --ref <addr> → ref <head> <addr>
		if idx := strings.Index(st, " --ref "); idx >= 0 && strings.HasPrefix(st, "put ") {
			head := strings.TrimSpace(st[4:idx])
			addr := strings.TrimSpace(st[idx+7:])
			st = "ref " + joinNonEmpty(head, addr)
		} else if idx := strings.Index(st, " -- "); idx >= 0 {
			head := strings.TrimSpace(st[:idx])
			body := strings.TrimSpace(st[idx+4:])
			if isCredentialPut(head) {
				// 凭据动词永远走 --name：<name> 是位置参数、正文是 payload 引用，
				// 两者不能挤在同一个 payload span 里（termcp 的规则）。
				// 正文已经是引用时保留引用名，否则调用方的 vars 会变成
				// 「传了但没人引用」而被拒绝。
				ref := body
				if !strings.HasPrefix(body, "$") || strings.ContainsAny(body, " \t") {
					ref = next(body)
				}
				st = credentialFlagForm(head) + " " + ref
			} else if strings.HasPrefix(body, "$") && !strings.ContainsAny(body, " \t") {
				// 正文本身就是一个引用：只去掉 `--`，不要把它挪进 args
				st = head + " " + body
			} else {
				st = head + " " + next(body)
			}
		}

		// -> 分隔符换成 --to
		if strings.Contains(st, " -> ") {
			st = strings.Replace(st, " -> ", " --to ", 1)
		}

		out = append(out, st)
		_ = i
	}

	joined := ""
	for i, st := range out {
		if i > 0 {
			op := "; "
			if i-1 < len(ops) && ops[i-1] != "" {
				op = " " + ops[i-1] + " "
			}
			joined += op
		}
		joined += st
	}
	return joined, nil, args
}

func joinNonEmpty(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, strings.TrimSpace(p))
		}
	}
	return strings.Join(keep, " ")
}

// isCredentialPut 判断 head 是不是 `remote-key put <name>` 这种形态。
func isCredentialPut(head string) bool {
	f := strings.Fields(head)
	return len(f) >= 2 && credentialVerbs[f[0]] && f[1] == "put"
}

// credentialFlagForm 把 `remote-key put <name>` 变成 `remote-key put --name <name>`。
func credentialFlagForm(head string) string {
	f := strings.Fields(head)
	if len(f) < 3 {
		return head
	}
	return f[0] + " put --name " + f[2]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
