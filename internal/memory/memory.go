// Package memory 定义记忆块与记忆树。
//
// 它只是数据结构：一段内容、它在哪、它是不是规则、怎么读出来。
// 不碰文件系统，不解析任何声明格式——因为存储层现在是 Git 裸仓库，
// 块就是一棵 tree 里的一个 entry。
package memory

import (
	"sort"
	"strings"
)

// Block 是一个可寻址的内容块。
type Block struct {
	// Project 是它所属的项目名（裸仓库目录名去掉 .git）。
	Project string
	// Branch 是它来自哪个分支。同一个仓库的不同分支是两份记忆，
	// 必须能同时存在——同一仓库的不同版本线各自独立。
	Branch string
	// Path 是树内路径，例如 "2026-10-29/225550-local.md"。
	Path string
	// Rule 为真表示这条是规则（tree entry 的 100755 位），每轮注入系统提示。
	// 为假表示是普通内容，只进倒排索引。
	Rule bool
	// Size 是字节数。
	Size int64
	// Time 是最后一次改动它的提交时间（Unix 秒）。零值表示还不知道。
	Time int64
	// Hidden 为真表示跳过检索（以点开头的隐藏目录、归档目录）。
	Hidden bool
	// Read 读出内容。它绑定到具体的仓库与分支。
	Read func() ([]byte, error)
}

// Addr 是逻辑地址，例如 "daily/2026-10-29/225550-local.md"。
//
// 不带分支：分支是「这份记忆的哪条版本线」，属于凭证的视角，
// 不属于地址本身。
func (b Block) Addr() string { return b.Project + "/" + b.Path }

// ID 是索引里的唯一键，带分支。
//
// 为什么索引必须带分支：同一个仓库的两个分支里可以有同名同路径的文件，
// 内容不同。只用 Addr 当键的话，后写进索引的会覆盖前一个——
// 而症状是「recall 搜出来的内容是另一个分支的」，且完全不报错。
func (b Block) ID() string { return MakeID(b.Project, b.Branch, b.Path) }

// Injectable 报告它是否每轮注入系统提示。
func (b Block) Injectable() bool { return b.Rule }

// Indexable 报告它是否进倒排索引。
//
// 规则块也进：它们是系统提示的一部分，但同样需要能用 recall
// 找到它们，否则「我知道有这条规则，但找不到它在哪」就成了常态。
func (b Block) Indexable() bool { return !b.Hidden }

// HiddenPath 判断一个路径是否属于「存在但不该进检索」的范围。
//
// 规则简单到不用解释：任何一段以点开头（.legacy-archive、.githooks、
// .gitignore）。人在仓库里放隐藏文件，意思就是「这不是正文」。
func HiddenPath(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// SplitAddrWithProjects 从候选项目名列表中，按最长前缀匹配剥离出项目与相对路径。
//
// 为什么这样做：代码不该写死项目必须是 "repo" 还是 "org/repo" 还是 "a/b/c"。
// 项目名由配置声明，这里只做最长匹配——完全不限制目录层级，单层多层一视同仁。
func SplitAddrWithProjects(addr string, projects []string) (project, path string, ok bool) {
	addr = strings.TrimPrefix(strings.TrimSpace(addr), "/")
	if addr == "" {
		return "", "", false
	}

	best := ""
	for _, p := range projects {
		p = strings.Trim(p, "/")
		if addr == p {
			return p, "", true
		}
		prefix := p + "/"
		if strings.HasPrefix(addr, prefix) && len(p) > len(best) {
			best = p
		}
	}

	if best == "" {
		// 匹配不到就说不匹配，绝不猜。
		//
		// 这里曾经有一个「取第一个斜杠前当项目名」的兜底，
		// 后果是：一个已声明但磁盘上还没建立的项目，它的地址匹配不到，
		// 于是被上层当成相对路径塞进 pwd，读到了**另一个仓库**里。
		// 不报错，只是读错地方。
		return "", "", false
	}
	return best, addr[len(best)+1:], true
}

// SplitAddr 保持向后兼容：默认取第一个斜杠前作为项目名。
func SplitAddr(addr string) (project, path string, ok bool) {
	addr = strings.TrimPrefix(strings.TrimSpace(addr), "/")
	i := strings.IndexByte(addr, '/')
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

// ProjectOf 从地址里取项目名（取第一个斜杠前；若地址无斜杠则返回它自身）。
func ProjectOf(addr string) string {
	addr = strings.TrimPrefix(strings.TrimSpace(addr), "/")
	if i := strings.IndexByte(addr, '/'); i > 0 {
		return addr[:i]
	}
	return addr
}

// Tree 是一个项目某个分支的全部块。
type Tree struct {
	// Project 是项目名。
	Project string
	// Branch 是分支名。
	Branch string
	// Blocks 按路径排序。
	Blocks []Block
}

// Sort 按地址排序，保证遍历顺序确定。
func (t *Tree) Sort() {
	sort.Slice(t.Blocks, func(i, j int) bool { return t.Blocks[i].Path < t.Blocks[j].Path })
}

// Key 返回 (项目, 分支)，供按分支索引树用。
func (t *Tree) Key() string { return t.Project + "@" + t.Branch }

// Find 按路径查找块。传入的 addr 可以带或不带项目前缀。
func (t *Tree) Find(addr string) (Block, bool) {
	addr = strings.TrimPrefix(addr, "/")
	addr = strings.TrimPrefix(addr, t.Project+"/")
	for _, b := range t.Blocks {
		if b.Path == addr {
			return b, true
		}
	}
	return Block{}, false
}

// Injectable 返回全部规则块。
func (t *Tree) Injectable() []Block {
	var out []Block
	for _, b := range t.Blocks {
		if b.Injectable() {
			out = append(out, b)
		}
	}
	return out
}

// Indexable 返回全部进索引的块。
func (t *Tree) Indexable() []Block {
	var out []Block
	for _, b := range t.Blocks {
		if b.Indexable() {
			out = append(out, b)
		}
	}
	return out
}

// Stats 汇总规模。
type Stats struct {
	Blocks      int
	Rules       int
	IndexBytes  int64
	SkippedSize int64
}

// Stats 汇总这棵树的规模。
func (t *Tree) Stats() Stats {
	var s Stats
	for _, b := range t.Blocks {
		s.Blocks++
		if b.Rule {
			s.Rules++
		}
		s.IndexBytes += b.Size
	}
	return s
}

// SplitID 把索引键拆回 project / branch / path 三段。
//
// 语法：`<project>@<ref>:<path>`，分隔符是冒号，与 Git 的
// `git show <ref>:<path>` 一致。
//
// 为什么必须用两个不同的分隔符——这条是踩过的：
// 早先写成 `project@branch/path`，分支与路径共用 `/`。后果是分支名里
// 不能有斜杠，于是「main 之下的 record 支流」这种结构无处表达，
// 只能把层级信息挤进一个词（`main-record`），而检索层又不得不靠
// 「分支名前缀」猜哪个分支装什么内容——两个语法域共用一个分隔符，
// 解析器只能靠猜。termcp 早期把 shell 与 DSL 混在一起就是这个病。
//
// 用 `:` 切开 ref 与 path 之后：
//   weft@main/record:2026-11-04/001.md
//        └───┬────┘ └──────┬───────┘
//         ref（可含 /）    path（另一个命名空间）
//
// 兼容旧地址：没有冒号时退回按第一个 `/` 切。旧索引键与旧调用方
// （插件、脚本、历史记忆里写下的 refs）全部继续可用。
func SplitID(id string) (project, branch, path string, ok bool) {
	at := strings.Index(id, "@")
	if at <= 0 {
		return "", "", "", false
	}
	project = id[:at]
	rest := id[at+1:]
	// 新语法：冒号切 ref 与 path。ref 内部的斜杠属于分支层级。
	if c := strings.Index(rest, ":"); c >= 0 {
		return project, rest[:c], rest[c+1:], true
	}
	// 旧语法兼容：没有冒号，第一个斜杠之前是分支。
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return project, rest, "", true
	}
	return project, rest[:slash], rest[slash+1:], true
}

// MakeID 是 SplitID 的逆运算，拼出新语法索引键。
//
// 统一走这里而不是各处手拼 "proj@"+branch+"/"+path：
// 地址格式只要有两处独立实现，就会有一处忘记跟着改。
func MakeID(project, branch, path string) string {
	return project + "@" + branch + ":" + path
}
