package server

import "strings"

// helpOverview 是空调用与 help 的默认输出：有什么、怎么选。
//
// 它要回答的不是"有哪些命令"，而是"我现在该用哪个"——
// 一个只列出动词名的手册，看完还是要猜。
func helpOverview() string {
	return strings.TrimSpace(`
zero-memcp：我的记忆库（实现 termcp 协议，单工具 term({cmd}) 入口）。

recall <text>            语义搜（返回地址 + 命中行前后摘要，多数情况看摘要就够了）
read <addr> [<addr>...]  读全文，可一次读多篇
grep <pattern>           正则扫全文（带前后上下文；搜精确串、数字、IP、函数名用它）
log [N]                  按时间倒序看最近 N 篇（缺省 5）
ls [date]                列出某天的全部切片
mailbox [ack <seq>]      待确认信箱（别人写了记忆时会投信，确认后才不打扰）
note <kind> [@session] -- <text>
                         写一条记忆。kind 只接受 fact / episode / preference / observation
status                   索引规模
commit [<p>[@<br>]] [-m msg]   显式封箱（缺省提交全部）
diff <p>@<br> [-> <p>@<br>]   分支比对 / 未审脏流提取
batch {"ops":[...]}      原子决议单（note/cp/mv/delete/tag/commit）

树外科手术（仅 root）：
branch                   列全部项目 × 分支
branch <p> <new> [from <src>]  分叉（缺省从默认分支）
tag <p> [<name> [<ref>]] 列 / 打 tag（-d <p> <name> 删）
cp <src>@<br>/<path> ... -> <dst>@<br>/[<path>]   跨分支复制
mv <src>@<br>/<path> ... -> <dst>@<br>/[<path>]   跨分支剪切
delete <p>@<br>/<path> [...]                     标记删除（commit 后生效）
kill <p>@<br>                                    吊销分支关联凭证
archive <p>@<br> [-> <p>@<as>]                   归档分支（ref 移到 refs/archived/）
reset <p>@<br> from <p>@<src>                    重置分支到另一分支（旧版自动归档）

怎么选：
  • 概念、推导、决定        → recall（例：recall 端口为什么定1729）
  • 精确符号、名字、数字    → grep（例：grep BlackEgg）
  • 最近在干嘛              → log 5
  • 读细节                  → read 2026-10-28/xxx.md
  • 要写下来                → note

help <verb>   看某个动词的细节
help --all    全部细节
`)
}

// helpVerb 返回单个动词的详细说明。
func helpVerb(verb string) (string, bool) {
	switch verb {
	case "recall":
		return strings.TrimSpace(`
recall <text>

语义搜。返回地址、分数，以及命中行前后约 240 字的摘要。

为什么给摘要而不是光秃秃的地址：只回 "addr (score)" 时看不出这篇讲什么，
必须逐个 read；读三四篇之后就会换 grep 去翻目录——不是不想用记忆工具，
是它每回答一个问题都要额外往返好几次。给摘要能把 N 次 read 压成 0 次。

例：recall 端口为什么定1729
`), true

	case "read":
		return strings.TrimSpace(`
read <addr> [<addr> ...]

读切片全文。可以一次读多篇，减少往返。

地址就是 recall / log / ls 返回的那个相对路径，例如 2026-10-28/023000-laptop-a1b2c3d4.md。

recall: off 的目录读不出来——声明写着"不许被翻到"，read 就不该给全文，
否则那句声明只是装饰。

例：read 2026-10-28/023000-laptop-a1b2c3d4.md
`), true

	case "grep":
		return strings.TrimSpace(`
grep <pattern>

正则扫全文，带前后行上下文。

BM25 不擅长纯符号和专有名词（QQ 号、IP、函数名、变量名），
这类东西用 grep 比 recall 准得多。这也是模型换 bash 去 rg 的头号原因——
给它原生的 grep，它就不用换工具了。

例：grep 1729
    grep func.*Server
`), true

	case "log":
		return strings.TrimSpace(`
log [N]

按时间倒序看最近 N 篇切片的日期、主题、摘要。缺省 5 篇。

「最近在干嘛」「上次说到哪了」用这个，不用去 ls 目录。

例：log
    log 20
`), true

	case "ls":
		return strings.TrimSpace(`
ls [date]

列出某一天的全部切片。缺省是今天（没有今天的数据就取最新一天）。

例：ls
    ls 2026-10-28
`), true

	case "mailbox":
		return strings.TrimSpace(`
mailbox [ack <seq> | clear]

信箱：别人写了记忆时会投一封信在这里。

为什么不是直接把记忆贴进你的上下文：
  1. 破坏前缀缓存：每来一条提醒，那一整段缓存就作废
  2. 强迫关注：你在专心修一个 bug，它硬塞一段"别人刚刚记了什么"
  3. 无法确认：塞进上下文并不代表你真的看过

信箱的逻辑：
  - 投信（未读）
  - 每轮如果信箱非空，只注入一行极短的提示："信箱有 N 封未确认"
  - 你随时敲 mailbox 看一眼清单
  - 看完敲 mailbox ack 确认，信就移出待办，那行极短提示也随之消失
  - 确认不删文件，文件一直在磁盘上

例：mailbox
    mailbox ack 3
    mailbox ack
`), true

	case "note":
		return strings.TrimSpace(`
note <kind> [@session] -- <text>

写一条记忆。-- 之后全部是正文，不解析。

kind 只接受四种：
  fact         事实（发生过、存在、是什么）
  episode      一段经过（做了什么事、结果如何）
  preference   偏好（喜欢什么、倾向怎么处理）
  observation  观察（注意到什么，未必有结论）

instruction / rule 会被安全拒绝：推断出来的指令不进记忆库。
记忆影响注意力，永不影响授权。

@session 可选，用来在面板里按会话分组。

例：note fact -- 端口定在 1729
    note episode @abc123 -- 今天把记忆服务重写了
`), true

	case "status":
		return strings.TrimSpace(`
status

索引规模：docs（文档数）与 terms（词条数）。

用来判断索引是不是活着、规模有没有异常。不返回内容。
`), true

	case "help":
		return strings.TrimSpace(`
help [<verb>] [--all]

不带参数：总览（跟空调用一样）
带动词：该动词的详细说明
--all：全部细节

例：help
    help note
    help --all
`), true
	}
	return "", false
}

// helpAll 把所有动词的细节一次给全。
func helpAll() string {
	var b strings.Builder
	b.WriteString(helpOverview())
	b.WriteString("\n\n")
	for _, v := range []string{"recall", "read", "grep", "log", "ls", "note", "status", "help"} {
		if d, ok := helpVerb(v); ok {
			b.WriteString("\n── " + v + " ")
			b.WriteString(strings.Repeat("─", max(0, 60-len(v))))
			b.WriteString("\n\n")
			b.WriteString(d)
			b.WriteString("\n")
		}
	}
	return b.String()
}
