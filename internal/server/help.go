package server

import "strings"

// helpOverview 是空调用与 help 的默认输出：有什么、怎么选。
//
// 它要回答的不是"有哪些命令"，而是"我现在该用哪个"——
// 一个只列出动词名的手册，看完还是要猜。
func helpOverview() string {
	return strings.TrimSpace(`
zero-memcp：一个持久记忆体。写入会落进本地持久记录，服务重启后仍可读取。

put [<tree>] -- $TEXT        把正文写进记忆树（正文通过 vars/args 传入，不和 DSL 混写）
put <tree> --ref <addr>      在树里追加已有记忆的引用，不复制正文
recall <text> [--scope tree] 语义回想；默认搜索长期记忆树，可指定范围
read <addr> [<addr>...]     读全文，可一次多篇
grep <pattern> [--scope T]  正则扫全文（精确符号、名字、数字）
log [N]                     最近记忆的时间线
ls [date]                   列出某天的记忆切片
trees <project>[@<branch>]  查看树目录
remind [<tree>]             锚定一棵树；树发生变化时收到通知
since <tree> <commit|empty> 取回自上次看到的提交以来的树差分
status                      索引规模

记忆同步（远端在配置里声明）：
remote [<project>]                    查看已配置的记忆副本
fetch <project> [<remote>]            从远端取回历史，不自动覆盖本地分支
push <project> [<remote>] <branch>   显式推送一个分支（一次一个，外部 git push 见 README）
sync <project> [<remote>]             查看本地与远端的差异
remote-key list | put | delete       root 配置 SSH 私钥（外部 push 用）
remote-secret list | put | delete    root 配置 HTTP token（外部 push 用）

push 必须显式指定分支，不会替你提交，也不会强推。

只有维护记忆结构时才需要的操作（普通 agent 不需要）：
commit [<p>[@<br>]] [-m msg]   显式折叠脏层进历史
diff <p>@<br> [-> <p>@<br>]    比对分支差异（审计用）
branch [<p> [<new> [from s]]]  列出或分叉分支
tag [-d] <p> [<name> [<ref>]]  轻量标签
cp / mv <src> -> <dst>         跨分支搬运切片
delete <p>@<br>/<path>...      删除切片
archive <p>@<br>               归档分支
reset <p>@<br> from <src>      重置分支到另一条分支
lock / unlock <p>@<br>         锁定/解锁分支（暂停非 root 访问）
chmod <p>@<br>/<path> mode     修改规则/内容属性
write <p>@<br>/<path> -- text  精确写文件（规则专用，须 root）
batch {"ops":[...]}            原子批处理（打包多步为一次提交）

怎么用：
  • 想起一件事             → recall
  • 想把新内容记住         → put
  • 想把某棵树的变化纳入注意→ remind <tree>
  • 想把记忆带到另一节点   → sync，再 fetch / push

help <verb>   看某个动词的细节
help --all    全部细节
`)
}

// helpVerb 返回单个动词的详细说明。
func helpVerb(verb string) (string, bool) {
	switch verb {
	case "put":
		return strings.TrimSpace(`
put [<tree>] -- $TEXT
put <tree> --ref <addr>

把正文放进记忆树。目标不带时，服务按 SKILL.md 的 stream 声明选择默认落点；
指定树路径时，正文作为新节点写入该子树。

文本必须通过 Termcp 的 vars / args 与 cmd 分开：
  cmd:  put team/chat -- $TEXT
  vars: {"TEXT":"正文，可以含空格、换行和 --"}

引用模式不复制正文：
  cmd: put team/chat --ref $1
  args: ["daily/2026-08-03/123456-local.md"]

put 返回的地址是稳定文件引用。写入会进入单线程脏层并可立即检索，由策略或 commit 折叠进历史。
`), true

	case "since":
		return strings.TrimSpace(`
since <project>@<branch>[/<tree>] <commit|empty>

列出该树自某个 commit 之后的增量变化。

这是断线续传的入口：保存你上次读到的 commit，重连后用它换回差量，
不必整棵树重读。SHA 相同则返回空——没动过的子树代价是零。

例：since blackegg/daily@chat/ctx/team 3f2a1b9
`), true

	case "remind":
		return strings.TrimSpace(`
remind [<project>[@<branch>[/<path>]]]

不带参数列出当前锚定；指定树路径则锚定它。
该分支对应子树变化时，在线客户端收到 tree 事件；断线后重连应重新锚定，
并用上次保存的 commit/tree 指针读取差分。
remind 只登记关注目标，不搬运正文、不分配任务。

例：
  remind blackegg/daily@chat/ctx/team
  remind
`), true

	case "trees":
		return strings.TrimSpace(`
trees <project>[@<branch>]

查看当前树目录，用路径选择 put / remind / recall 的范围。
`), true

	case "lock", "unlock":
		return strings.TrimSpace(`lock   <project>@<branch>
unlock <project>@<branch>

锁住或解锁一条分支：非 root 凭证对它的一切读写都被拒绝，root 不受影响。

锁的用途是「停下来让 root 处理」——分拣、搬运、归档、重置。
所以 root 必须仍然能进去：一道连 root 都挡住的闸门等于把分支堵死。

锁状态存在仓库内的 zero-memcp/locks/，不进 Git 历史、不会被 clone/push 看到，
重启后仍然有效。branch 列表里被锁的分支会标 [已锁定]。

回收一条分支的典型顺序：
  lock   blackegg/me@chat              # agent 下一个请求就 403
  ls     blackegg/me@chat              # 分拣：看清楚有什么
  cp     blackegg/me@chat/x.md -> blackegg/me@zero/lessons/
  archive blackegg/me@chat             # 收尾：内容全留，引用移入归档命名空间
  # 或者决定继续用：unlock blackegg/me@chat
`), true

	case "chmod":
		return strings.TrimSpace(`chmod <project>[@<branch>]/<path> rule|content

改动一个块的模式位：

  rule     100755，每轮注入系统提示（法则、身份、硬约束）
  content  100644，普通内容，只在被 recall 时出现

write 与 put 都只能写 content。要把一条已有内容提升为规则，用 chmod；
它保留正文，只改模式位。改完要 commit 才生效。

例：chmod blackegg/me@zero/rules/01.md rule
`), true

	case "remote-secret":
		return strings.TrimSpace(`remote-secret list
remote-secret put <name> -- $TOKEN
remote-secret delete <name>

root 管理 HTTP 推送用的令牌。保存在 system/secrets/<name>（0600），只回显名字，从不回显内容。
GitHub 用 http_user = "x-access-token"；GitLab 等用你的账号名。
明文 HTTP 只允许回环地址：token 不能被发到网络上去。

例：remote-secret put gh -- $TOKEN
`), true

	case "remote-key":
		return strings.TrimSpace(`remote-key list
remote-key put <name> -- $PRIVATE_KEY
remote-key delete <name>

私钥只能由 root 通过 vars/args 传入，保存到 system/sshkeys/<name>.pem（0600），从不回显。
只接受未加密 OpenSSH PEM 私钥。远端必须配置 host_key = "SHA256:..." 指纹。

例：remote-key put github-deploy -- $KEY
`), true

	case "remote", "fetch", "push", "sync":
		return strings.TrimSpace(`
remote [<project>]                    列出项目声明的远端
fetch <project> [<remote>]        取回远端历史到 refs/remotes/<remote>/*
push <project> [<remote>] <branch>   推送一个分支（SSH 内置实现，不批量）
sync <project> [<remote>]             报告领先/落后/分叉
remote-key list | put | delete       root 配置 SSH 私钥

配置在 TOML 里：

  [[remote]]
  project    = "blackegg/daily"
  name       = "github"
  url        = "git@github.com:private/daily.git"
  push       = ["main", "chat"]     # 空 = 不可推
  fetch_only = false                # true = 只读远端
  ssh_key    = "github-deploy"     # SSH push 的 root 私钥名
  host_key   = "SHA256:..."         # 必须 pin，不信任未知 SSH host key

HTTP 远端（HTTPS）用 token：

  [[remote]]
  project    = "blackegg/daily"
  name       = "github"
  url        = "https://github.com/org/daily.git"
  push       = ["zero"]
  http_user  = "x-access-token"     # GitHub 的约定
  http_token = "gh"                 # remote-secret 保存的名字，不是 token 本身

四条纪律：
  • 不自动 push：写入只落本地，推送必须显式调用
  • 不隐式 commit：有未提交内容时 push 直接拒绝并说明条数
  • 不强推：+refspec 一律拒绝（记忆历史只追加）
  • 冲突不静默：分叉时保留两边并报错，不自动 merge/rebase

fetch / push / sync 全部由内置传输完成，不调用系统 git：
SSH 走 Go SSH，HTTP(S) 走 smart-HTTP，本地裸仓库路径直接读写对象库。
三条传输共用同一套协商、pack 生成与快进判定。
凭证只存在服务端密钥库（remote-key / remote-secret），既不进 TOML，也不转发。
fetch 优先用协议 v2（ls-refs + fetch），不支持时退回 v0，并且会带上本地分支尖做 have 协商。
gc 用内置打包器写出 pack + .idx v2，同样不需要系统 git。

例：
  sync blackegg/daily github
  push blackegg/daily github main chat
  fetch blackegg/daily github
`), true

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
grep <pattern> [--scope <tree>]

正则扫全文，带前后行上下文。

BM25 不擅长纯符号和专有名词（QQ 号、IP、函数名、变量名），
这类东西用 grep 比 recall 准得多。这也是模型换 bash 去 rg 的头号原因——
给它原生的 grep，它就不用换工具了。

--scope 收窄范围，可写 project、project@branch 或 project@branch/subtree：
记忆树大了之后，不分范围的全量扫会把命中堆到与本次无关的项目上。

例：grep 1729
    grep 1729 --scope daily
    grep func.*Server --scope blackegg/me@zero/rules
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
    help put
    help --all
`), true

	case "commit":
		return strings.TrimSpace(`
commit [<project>[@<branch>]] [-m <msg>]

把脏层折叠成 Git 提交，推进分支 ref。

put 只负责投递：它把内容写进目录级脏层就返回，不代表历史已经前进。
折叠要么由显式 commit 触发，要么由后台 [commit] 策略（条数/时间）自动完成。

不带参数时提交所有项目；指定项目/分支时只提交那一支。

例：commit
    commit blackegg/daily@chat
    commit blackegg/daily@chat -m "整理端口笔记"
`), true

	case "diff":
		return strings.TrimSpace(`
diff <project>@<branch>
diff <project>@<branch> -> <project>@<branch>

分支比对：列出两边路径的差异。

单分支形式列出该分支相对其父提交的变更；箭头形式对比两条分支。

审计 Agent 的入口：diff audited -> raw 提取未审内容。

例：diff blackegg/daily@chat
    diff blackegg/daily@audited -> blackegg/daily@raw
`), true

	case "branch":
		return strings.TrimSpace(`
branch
branch <project>
branch <project> <new> [from <src>]

不带参数：列出所有项目的分支
带项目：列出该项目的分支
带新分支名：从 src（缺省当前分支）分叉出一条新分支

分叉是零字节操作：只写一个 ref，不复制任何对象。

例：branch
    branch blackegg/daily
    branch blackegg/daily review from chat
`), true

	case "tag":
		return strings.TrimSpace(`
tag <project> [<name> [<ref>]]
tag -d <project> <name>

轻量标签：给某个提交起一个稳定的名字。

不带 name 时列出该项目的全部标签。
-d 删除标签，但不删内容——内容在分支里，标签只是一个指针。

例：tag blackegg/daily
    tag blackegg/daily v1 chat
    tag -d blackegg/daily v1
`), true

	case "cp", "mv":
		return strings.TrimSpace(`
cp <src>@<br>/<path> ... -> <dst>@<br>/[<path>]
mv <src>@<br>/<path> ... -> <dst>@<br>/[<path>]

跨分支复制（cp）或移动（mv）切片。

这是审计与提纯的搬运工具：把 chat 分支上审过的内容搬进 main，
不必先把内容读出来再写一遍（那样会丢掉原来的文件时间与模式位）。

mv = cp + delete，两步在同一个提交边界内完成。

例：cp blackegg/daily@chat/x.md -> blackegg/daily@main/lessons/
    mv blackegg/me@chat/x.md -> blackegg/me@zero/lessons/
`), true

	case "delete":
		return strings.TrimSpace(`
delete <project>@<branch>/<path> [<path> ...]

从指定分支删除切片。可一次删多个。

删除同样进脏层，要 commit 才真正从历史里消失（在下一个提交里不再出现）。

例：delete blackegg/daily@chat/2026-10-28/old.md
`), true

	case "archive":
		return strings.TrimSpace(`
archive <project>@<branch> [-> <project>@<as>]

归档一条分支：把它的 ref 移入 refs/archived/，内容全部保留。

归档不是删除：对象还在、历史还在，只是从分支列表里移走，
不再出现在 clone/push 的默认引用里。

典型用途：一个 agent 的任务结束了，它的分支不该继续被当作活跃版本线。

例：archive blackegg/me@chat
    archive blackegg/me@chat -> blackegg/me@chat-2026-10
`), true

	case "reset":
		return strings.TrimSpace(`
reset <project>@<branch> from <project>@<src>

把一条分支重置到另一条分支的当前位置。

这是覆盖历史，只能由 root 显式执行——push 永远不做这件事。
重置会清掉目标分支的脏层：重置就是放弃所有未提交状态。

例：reset blackegg/daily@zero from blackegg/daily@import
`), true

	case "write":
		return strings.TrimSpace(`
write <project>[@<branch>]/<path> -- <content>

把内容写到指定路径，文件名与位置完全由你决定。

与 put 的区别：put 写的是「切片」（自动时间戳文件名 + frontmatter），
write 写的是「这个路径上就该是这段内容」。它存在的理由是规则文件：
SKILL.md 的 frontmatter 决定注入与索引，那不可能靠 put 的自动命名表达。

仅 root：能改 SKILL.md 的人可以改「谁看见什么」，比改内容本身危险。

正文同样走 vars/args：
  cmd:  write blackegg/me@zero/SKILL.md -- $BODY
  vars: {"BODY": "---\nname: me\n---\n"}

例：write blackegg/me@zero/rules/01.md -- $TEXT
`), true

	case "batch":
		return strings.TrimSpace(`
batch {"ops":[{...}, ...]}

把多个写操作打包成一次提交。

为什么需要它：审计 Agent 常常要一次性搬运一批切片，
逐个执行会产生几十个中间提交，历史被切碎，而且中途失败会留下半吊子状态。
batch 要么全部成功，要么一个都不落。

支持的操作：put / cp / mv / delete / tag / commit。

例：batch {"ops":[{"op":"delete","addr":"daily@chat/x.md"},{"op":"commit"}]}
`), true

	case "config":
		return strings.TrimSpace(`
config show
config path
config put <TOML>

查看或更新服务配置文件（仅 root）。

config show 显示当前加载的有效配置。
config put 原子覆写配置文件并触发重载（支持通过 vars 传大段 TOML）。

例：config show
`), true

	case "reload":
		return strings.TrimSpace(`
reload

重新扫描所有项目并重建倒排索引（仅 root）。

外部直接改动裸仓库（如人手敲 git commit、或外部工具推送进来）后，
可以用此动词强制刷新内存状态，无需重启进程。
`), true

	case "whoami":
		return strings.TrimSpace(`
whoami

回显当前调用方的凭证主体、有效角色、能力位与挂载范围。

用于自省与调试：排查「为什么我写不进去」时先看这张卡片。
`), true

	case "users":
		return strings.TrimSpace(`
users

列出配置中声明的用户与访问控制摘要（仅 root）。
`), true

	}
	return "", false
}

// helpAll 把所有动词的细节一次给全。
func helpAll() string {
	var b strings.Builder
	b.WriteString(helpOverview())
	b.WriteString("\n\n")
	for _, v := range []string{"recall", "read", "grep", "log", "ls", "put", "write", "remind", "trees", "since", "status", "chmod", "lock", "unlock", "remote-key", "remote-secret", "remote", "fetch", "push", "sync", "commit", "diff", "branch", "tag", "cp", "mv", "delete", "archive", "reset", "batch", "config", "reload", "whoami", "users", "help"} {
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
