package server

import "strings"

// 这一组处理「读出来给人/给模型看」时的呈现形态。
//
// 起因（2026-11-28）：写进仓库的每条记忆都带一段 frontmatter
//
//	---
//	source: weft
//	endpoint: laptop
//	created: 2026-11-28T10:39:19+08:00
//	---
//
// 实测正好 95 个字符。而 agent 侧对工具返回值做摘要时只取**前 120 个字符**，
// 于是摘要窗口里只剩这段元数据，正文一个字都不剩——工具返回值的第一屏
// 被「这条从哪来、什么时候写的」占满，而调用方要的是「这篇讲什么」。
//
// recall 早就剥了这段（snippet 里那句注释：模型要看的是内容，不是元数据），
// 只有 read 一直原样吐整份文件。所以缺口只在一个动词上。

// noteFrontmatter 是一份记忆的溯源字段。
type noteFrontmatter struct {
	Kind     string
	Source   string
	Endpoint string
	Created  string
	Session  string
}

// splitFrontmatter 把内容拆成「溯源字段」与「正文」。
//
// 拆不出来时返回 ok=false，正文原样返回：宁可多给一行元数据，
// 也不能把一份没有 frontmatter 的普通文件砍掉开头。
func splitFrontmatter(content string) (fm noteFrontmatter, body string, ok bool) {
	if !strings.HasPrefix(content, "---\n") {
		return fm, content, false
	}
	// 找结束标记：紧跟在换行之后的 ---。用 "\n---\n" 而不是 "\n---"，
	// 否则正文里一行 "----" 也会被当成结束。
	end := strings.Index(content[4:], "\n---\n")
	if end < 0 {
		return fm, content, false
	}
	raw := content[4 : 4+end]
	body = content[4+end+5:]
	for _, line := range strings.Split(raw, "\n") {
		k, v, has := strings.Cut(line, ":")
		if !has {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "kind":
			fm.Kind = v
		case "source":
			fm.Source = v
		case "endpoint":
			fm.Endpoint = v
		case "created":
			fm.Created = v
		case "session":
			fm.Session = v
		}
	}
	return fm, body, true
}

// provenanceLine 把溯源字段压成一行，放在正文之后。
//
// 为什么放在**末尾**而不是开头：摘要窗口只取前 120 个字符，而前 120 个
// 字符该给标题和正文。放在开头等于把刚拆掉的那段又原样装回去。
//
// 为什么不能干脆不显示：source/endpoint/created 是「这条从哪来」的唯一
// 凭证，写回时服务端会重新生成它们（created 变成当前时间），所以读出来
// 不给看就等于把原始时间戳藏了起来。
func provenanceLine(fm noteFrontmatter) string {
	var parts []string
	if fm.Kind != "" {
		parts = append(parts, "kind="+fm.Kind)
	}
	if fm.Source != "" {
		parts = append(parts, "source="+fm.Source)
	}
	if fm.Endpoint != "" {
		parts = append(parts, "endpoint="+fm.Endpoint)
	}
	if fm.Created != "" {
		parts = append(parts, "created="+fm.Created)
	}
	if fm.Session != "" {
		parts = append(parts, "session="+fm.Session)
	}
	if len(parts) == 0 {
		return ""
	}
	return "-- " + strings.Join(parts, " · ")
}

// renderNote 把一份记忆渲染成「正文 + 溯源一行」。
//
// raw 为真时原样返回（字节精确，写回、比对、排障都要用到）。
func renderNote(content string, raw bool) string {
	if raw {
		return content
	}
	fm, body, ok := splitFrontmatter(content)
	if !ok {
		return content
	}
	body = strings.TrimSpace(body)
	line := provenanceLine(fm)
	if line == "" {
		return body
	}
	if body == "" {
		return line
	}
	return body + "\n\n" + line
}
