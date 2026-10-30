// Package manifest 解析 SKILL.md 的声明部分。
//
// 只有 100755 的条目（规则）才会被当成 manifest 解析——
// 普通内容（100644）写了也不生效，避免切片伪造写入规则。
package manifest

import (
	"strings"
)

// Policy 是从 SKILL.md 里读出的动作策略。
type Policy struct {
	// Name 是项目/模块名。
	Name string
	// Target 是写入目标仓库名。空表示用项目自己。
	Target string
	// Layout 控制路径组织：date (YYYY-MM-DD/HHMMSS.sss) 或 flat (HHMMSS.sss)。
	// 缺省 date。
	Layout string
	// Kinds 是允许写入的分类列表。空表示用默认白名单。
	Kinds []string
	// Inject 报告是否注入系统提示：every_turn / never（缺省根据文件名与 mode）
	Inject string
	// Index 报告是否进倒排索引：full / on_demand / none
	Index string
	// Stream 标记这是「流水」项目：会话记录、日常笔记默认落这里。
	//
	// 为什么需要它：note 不带目标时，服务端要在多个可写项目里挑一个。
	// 早先是「挑第一个可写的」——那是个偶然成立的默认，改个挂载表
	// 顺序就可能把流水写进身份库，而且不报错。
	//
	// 现在由项目自己声明：谁写 stream: true，谁就是流水归宿。
	// 一个都不声明时，才回退到「唯一可写项目」；多于一个则报错，
	// 逼调用方显式指定——沉默地选错，比报错难查得多。
	Stream bool
}

// Default 返回未声明时的兜底策略。
func Default(project string) Policy {
	return Policy{
		Name:   project,
		Target: project,
		Layout: "date",
		Kinds:  []string{"fact", "episode", "preference", "observation"},
	}
}

// Parse 从 Markdown 文本的 YAML frontmatter 提取策略。
// 不带 frontmatter 或没有 write 节的，返回 Default。
func Parse(project string, raw []byte) Policy {
	p := Default(project)
	s := string(raw)
	if !strings.HasPrefix(s, "---") {
		return p
	}

	lines := strings.Split(s, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return p
	}

	fmLines := lines[1:end]
	inWrite := false

	for _, line := range fmLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// 顶层键
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			k, v, ok := strings.Cut(trimmed, ":")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k == "write" {
				inWrite = true
				continue
			}
			inWrite = false
			switch {
			case k == "name" && v != "":
				p.Name = v
			case k == "inject":
				p.Inject = strings.ToLower(v)
			case k == "index":
				p.Index = strings.ToLower(v)
			case k == "stream":
				p.Stream = v == "true" || v == "yes" || v == "1"
			}
			continue
		}

		// write: 下的缩进字段
		if inWrite {
			k, v, ok := strings.Cut(trimmed, ":")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			switch k {
			case "target":
				if v != "" {
					p.Target = v
				}
			case "layout":
				if v == "flat" || v == "date" {
					p.Layout = v
				}
			case "kinds":
				// 支持 [a, b, c] 或多行
				v = strings.Trim(v, "[]")
				var kinds []string
				for _, item := range strings.Split(v, ",") {
					item = strings.TrimSpace(item)
					if item != "" {
						kinds = append(kinds, item)
					}
				}
				if len(kinds) > 0 {
					p.Kinds = kinds
				}
			}
		}
	}

	return p
}

// AllowsKind 判断某分类是否允许写入。
func (p Policy) AllowsKind(kind string) bool {
	for _, k := range p.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}
