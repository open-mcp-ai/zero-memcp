package server

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// 参数替换：把 DSL 骨架与正文数据分开。
//
// 为什么必须分开：正文里可能包含 "--"、引号、换行、任意标点。
// 如果它和指令拼在同一行里再由服务端切分，切分器迟早会把正文当成语法。
// 那不只是解析错误——它会把用户的数据变成命令。
//
// 两条硬规则，都来自「引用必须独占一个词」：
//   - 一个词要么完全是引用（$NAME），要么完全不含引用；
//   - 引用所在的位置就是那段数据的全部，前后不允许再拼别的字。
//
// 于是替换只能发生在「整词」粒度上：值里有什么字符都不影响语法，
// 因为它替换掉的是一个完整的词，而不是插进某个词中间。

// expandArgs 把 cmd 里的 $NAME / $N 用 vars / args 替换掉。
//
// 返回值里的 hasRef 表示是否真有引用被替换（用于判断是否需要该校验）。
func expandArgs(cmd string, vars map[string]string, args []string) (string, error) {
	tokens := strings.Fields(cmd)
	usedVar := map[string]bool{}
	usedArg := map[int]bool{}

	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		name, pos, isRef, err := parseRef(tok)
		if err != nil {
			return "", err
		}
		if !isRef {
			// 非引用词里出现 $ 一律拒绝：那多半是想把引用塞进句子里，
			// 而「塞进去」正是注入的入口。
			if strings.Contains(tok, "$") {
				return "", fmt.Errorf("reference %q must occupy a whole word: values replace an entire word and cannot be combined with other text", tok)
			}
			out = append(out, tok)
			continue
		}
		if name != "" {
			v, ok := vars[name]
			if !ok {
				return "", fmt.Errorf("cmd references $%s, but vars has no such name", name)
			}
			usedVar[name] = true
			out = append(out, encodePayload(v))
			continue
		}
		if pos <= 0 || pos > len(args) {
			return "", fmt.Errorf("cmd references $%d, but args has only %d values", pos, len(args))
		}
		usedArg[pos] = true
		out = append(out, encodePayload(args[pos-1]))
	}

	// 传了却没人用的参数必须报错。
	//
	// 静默丢掉一个参数，调用方会以为它生效了——那种错误不会报错，
	// 只会让结果在很久以后显得莫名其妙。
	for name := range vars {
		if !usedVar[name] {
			return "", fmt.Errorf("vars value %s is not referenced by cmd (argument was discarded)", name)
		}
	}
	for i := range args {
		if !usedArg[i+1] {
			return "", fmt.Errorf("args value %d is not referenced by cmd (argument was discarded)", i+1)
		}
	}
	return strings.Join(out, " "), nil
}

// parseRef 判断一个词是不是引用。
//
// 支持 $NAME 与 $N（1 起）。$0 拒绝：位置参数从 1 开始，
// 0 只会出现在「想当然」的调用里，静默接受它等于让一次错误看起来成功。
func parseRef(tok string) (name string, pos int, isRef bool, err error) {
	if !strings.HasPrefix(tok, "$") {
		return "", 0, false, nil
	}
	body := tok[1:]
	if body == "" {
		return "", 0, false, nil
	}
	// ${10} 形式：位数多了以后必须有括号，否则 $10 会和 $1 混淆。
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = body[1 : len(body)-1]
	}
	if body == "" {
		return "", 0, false, fmt.Errorf("empty reference %q", tok)
	}
	numeric := true
	for _, c := range body {
		if c < '0' || c > '9' {
			numeric = false
			break
		}
	}
	if numeric {
		n := 0
		for _, c := range body {
			n = n*10 + int(c-'0')
		}
		if n == 0 {
			return "", 0, true, fmt.Errorf("$0 is not a valid reference: positional arguments start at $1")
		}
		return "", n, true, nil
	}
	return body, 0, true, nil
}

// encodePayload 把一段任意正文编码成一个 DSL 安全的原子词。
//
// 为什么不能直接 strings.Join 再拆回去：值里的空格、换行、"--" 都會
// 改变语法。把它编码成无空白原子词，解析完指令骨架后，再只在正文槽
// 还原原值；数据从头到尾都没有机会参与 DSL 解析。
func encodePayload(value string) string {
	return "~ZMEMARG~" + base64.RawURLEncoding.EncodeToString([]byte(value)) + "~END~"
}

// restorePayload 把原子正文槽还原成原始字节。
func restorePayload(value string) string {
	const prefix = "~ZMEMARG~"
	const suffix = "~END~"
	var out strings.Builder
	cursor := 0
	for cursor < len(value) {
		relStart := strings.Index(value[cursor:], prefix)
		if relStart < 0 {
			out.WriteString(value[cursor:])
			break
		}
		start := cursor + relStart
		out.WriteString(value[cursor:start])
		bodyStart := start + len(prefix)
		relEnd := strings.Index(value[bodyStart:], suffix)
		if relEnd < 0 {
			out.WriteString(value[start:])
			break
		}
		end := bodyStart + relEnd
		encoded := value[bodyStart:end]
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			out.WriteString(value[start : end+len(suffix)])
		} else {
			// 只扫原始模板一次：解码后的正文不再二次扫描。
			// 否则正文里长得像 marker 的字串会被当作嵌套引用改写。
			out.Write(decoded)
		}
		cursor = end + len(suffix)
	}
	return out.String()
}

// ExpandArgs 是给 pkg/zeromem 等内嵌调用者使用的协议入口。
func ExpandArgs(cmd string, vars map[string]string, args []string) (string, error) {
	return expandArgs(cmd, vars, args)
}
