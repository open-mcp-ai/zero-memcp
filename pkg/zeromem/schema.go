package zeromem

// memToolDescription 是给模型看的用法说明。
//
// 写法取舍：不说「这是一个记忆管理系统」那种空话，直接说什么时候用哪个
// 动词。模型读这段话的唯一目的就是决定「我现在该敲哪一行」。
const memToolDescription = `我的持久记忆体。写入即入队，立即可读；commit 后进历史。收一行 DSL：

  put [<tree>] -- $TEXT    写入记忆（正文走 vars/args，不要拼进 cmd）
  put <tree> --ref <addr>  在树里追加已有记忆的引用
  recall <text> [--scope]  语义联想（返回地址 + 摘要片段）
  read <addr> [<addr>...]  读全文，可一次多篇
  grep <regex> [--scope]   正则扫全文（找符号/数字/专名用它，比 recall 准）
  log [N]                  最近 N 篇
  ls [date]                列出某天
  trees <project>[@<br>]   查看树目录
  remind [<tree>]          锚定一棵树；它变动时收到通知
  since <tree> <commit>    取回自上次提交以来的树差分
  status                   索引规模

正文用 vars / args 传，引用必须独占一个词：
  {"cmd": "put team/chat -- $TEXT", "vars": {"TEXT": "正文"}}

多分支是多人格/多端点协作的物理基础：
  commit / branch / diff / cp / mv / tag / delete / archive / reset / lock / unlock / batch

什么时候用哪个：问「关于 X 我之前怎么想的」→ recall；
找精确串（IP、端口、函数名）→ grep；问「最近做到哪了」→ log。`

// memInputSchema 是 mem 工具的参数表。
func memInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cmd": map[string]any{
				"type":        "string",
				"description": "DSL 命令行，例如 'recall napcat' 或 'put -- $TEXT'。正文放进 vars/args，用 $NAME / $1 引用。",
			},
			"args": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "位置参数，供 cmd 里的 $1、$2 引用。引用必须独占一个词，值不做转义。",
			},
			"vars": map[string]any{
				"type":        "object",
				"description": "命名参数，供 cmd 里的 $NAME 引用。传了就必须被引用。",
			},
		},
		"required": []string{"cmd"},
	}
}
