// Package config 读取 zero-memcp 的配置文件。
//
// 为什么自己写一个 TOML 子集而不是引一个库：这个项目的外部依赖是零，
// 而我们需要的东西少得可怜——分节、键值、字符串、整数、布尔、
// 字符串数组、[[数组表]]。用不到的那些（日期、浮点、多行字符串、
// 内联表、点号键、elaborate 转义）全部不支持，遇到就报错。
//
// 「遇到就报错」比「悄悄忽略」重要：一个写错的配置项如果被静默跳过，
// 结果是服务照常启动、权限却不是你写的那样，而没有任何提示。
package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Document 是解析后的配置：分节 -> 键值，以及顶层数组表。
type Document struct {
	// Sections 是 [name] 形式的分节，值是该节下的键值。
	Sections map[string]map[string]Value
	// Arrays 是 [[name]] 形式的数组表，按出现顺序。
	Arrays map[string][]map[string]Value
	// Order 记录 [name] 与 [[name]] 的出现顺序，只用于让错误信息
	// 能指出「第几个 [[user]] 有问题」。
	Order []string
}

// Value 是一个配置值。只保留我们真正需要的四种。
type Value struct {
	Str  string
	Int  int64
	Bool bool
	List []string
	Kind Kind
	Line int
}

// Kind 说明这个值是什么类型。
type Kind int

const (
	KindString Kind = iota
	KindInt
	KindBool
	KindList
)

func (v Value) String() string {
	switch v.Kind {
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case KindList:
		return "[" + strings.Join(v.List, ", ") + "]"
	}
	return v.Str
}

// Parse 解析整份配置。
func Parse(data []byte) (*Document, error) {
	doc := &Document{
		Sections: map[string]map[string]Value{},
		Arrays:   map[string][]map[string]Value{},
	}

	var (
		section    string // 当前 [name]
		sectionMap map[string]Value
		arrayName  string // 当前 [[name]]
	)

	lines := strings.Split(string(data), "\n")
	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}

		// [[name]]
		if strings.HasPrefix(line, "[[") {
			if !strings.HasSuffix(line, "]]") {
				return nil, fmt.Errorf("第 %d 行：数组表声明应以 ]] 结尾，得到 %q", lineNo, line)
			}
			name := strings.TrimSpace(line[2 : len(line)-2])
			if err := validName(name); err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
			}
			m := map[string]Value{}
			doc.Arrays[name] = append(doc.Arrays[name], m)
			doc.Order = append(doc.Order, "[["+name+"]]")
			section = ""
			sectionMap = nil
			arrayName = name
			// 用指向最后一个元素的指针语义：直接改 doc.Arrays[name] 里的 map
			sectionMap = doc.Arrays[name][len(doc.Arrays[name])-1]
			continue
		}

		// [name]
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("第 %d 行：分节声明应以 ] 结尾，得到 %q", lineNo, line)
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if err := validName(name); err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
			}
			if _, ok := doc.Sections[name]; ok {
				return nil, fmt.Errorf("第 %d 行：分节 %q 重复声明", lineNo, name)
			}
			doc.Sections[name] = map[string]Value{}
			section = name
			sectionMap = doc.Sections[name]
			arrayName = ""
			doc.Order = append(doc.Order, "["+name+"]")
			continue
		}

		// key = value
		key, valText, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("第 %d 行：缺 = 号，得到 %q", lineNo, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("第 %d 行：键名为空", lineNo)
		}
		if sectionMap == nil {
			return nil, fmt.Errorf("第 %d 行：键 %q 出现在任何分节之前", lineNo, key)
		}
		val, err := parseValue(strings.TrimSpace(valText), lineNo)
		if err != nil {
			return nil, err
		}
		if _, dup := sectionMap[key]; dup {
			where := section
			if arrayName != "" {
				where = "[[" + arrayName + "]] 的某一项"
			}
			return nil, fmt.Errorf("第 %d 行：%s 里的键 %q 重复", lineNo, where, key)
		}
		sectionMap[key] = val
	}
	return doc, nil
}

// stripComment 去掉行尾注释，但不碰引号里的 #。
//
// 这个细节容易漏：url = "git@host:repo#frag" 里的 # 不是注释。
func stripComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			if inStr && i+1 < len(line) {
				i++ // 跳过被转义的字符
			}
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return line[:i]
			}
		}
	}
	return line
}

func parseValue(s string, lineNo int) (Value, error) {
	if s == "" {
		return Value{}, fmt.Errorf("第 %d 行：缺值", lineNo)
	}

	// 字符串
	if strings.HasPrefix(s, `"`) {
		v, rest, err := parseString(s)
		if err != nil {
			return Value{}, fmt.Errorf("第 %d 行：%w", lineNo, err)
		}
		if strings.TrimSpace(rest) != "" {
			return Value{}, fmt.Errorf("第 %d 行：字符串后面还有东西：%q", lineNo, rest)
		}
		return Value{Str: v, Kind: KindString, Line: lineNo}, nil
	}

	// 字符串数组
	if strings.HasPrefix(s, "[") {
		list, err := parseList(s)
		if err != nil {
			return Value{}, fmt.Errorf("第 %d 行：%w", lineNo, err)
		}
		return Value{List: list, Kind: KindList, Line: lineNo}, nil
	}

	// 布尔
	switch s {
	case "true":
		return Value{Bool: true, Kind: KindBool, Line: lineNo}, nil
	case "false":
		return Value{Bool: false, Kind: KindBool, Line: lineNo}, nil
	}

	// 整数
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return Value{Int: n, Kind: KindInt, Line: lineNo}, nil
	}

	return Value{}, fmt.Errorf("第 %d 行：无法识别的值 %q（字符串要加引号）", lineNo, s)
}

func parseString(s string) (string, string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", "", fmt.Errorf("字符串以反斜杠结尾")
			}
			i++
			switch s[i] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return "", "", fmt.Errorf("不支持的转义 \\%c", s[i])
			}
		case '"':
			return b.String(), s[i+1:], nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", "", fmt.Errorf("字符串没有闭合的引号")
}

func parseList(s string) ([]string, error) {
	if !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("数组应以 ] 结尾，得到 %q", s)
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" {
		return nil, nil
	}
	var out []string
	for {
		body = strings.TrimSpace(body)
		if body == "" {
			break
		}
		if !strings.HasPrefix(body, `"`) {
			return nil, fmt.Errorf("数组元素必须是加引号的字符串，得到 %q", body)
		}
		v, rest, err := parseString(body)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		if !strings.HasPrefix(rest, ",") {
			return nil, fmt.Errorf("数组元素之间要有逗号，得到 %q", rest)
		}
		body = rest[1:]
	}
	return out, nil
}

func validName(name string) error {
	if name == "" {
		return fmt.Errorf("分节名为空")
	}
	for _, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
		if !ok {
			return fmt.Errorf("分节名 %q 含非法字符 %q（只允许字母数字 _ - .）", name, c)
		}
	}
	return nil
}
