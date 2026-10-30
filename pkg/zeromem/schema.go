package zeromem

// memToolDescription 是给模型看的用法说明。
//
// 写法取舍：不说「这是一个记忆管理系统」那种空话，直接说什么时候用哪个
// 动词。模型读这段话的唯一目的就是决定「我现在该敲哪一行」。
const memToolDescription = `我的长期记忆库。收一行 DSL：

  recall <text>            语义联想（返回地址 + 摘要片段）
  read <addr> [<addr>...]  读全文，可一次多篇
  grep <regex>             正则扫全文（找符号/数字/专名用它，比 recall 准）
  log [N]                  最近 N 篇
  ls [date]                列出某天
  note <kind> -- <text>    追加一条（fact/episode/preference/observation）
  status                   索引规模

分支与提交（多分支是多人格/多端点协作的物理基础）：
  commit [<p>[@<br>]] [-m msg]   显式封箱；引擎不会自己提交
  branch [<p> [<new> [from <src>]]]  列 / 分叉
  diff <p>@<br> [-> <p>@<br>]    分支比对 / 未审脏流提取
  cp|mv <src>@<br>/<path> ... -> <dst>@<br>/[<path>]
  tag / delete / archive / reset / kill / batch

什么时候用哪个：问「关于 X 我之前怎么想的」→ recall；
找精确串（IP、端口、函数名）→ grep；问「最近做到哪了」→ log。
写完一条事实性内容后，需要它落到历史里时调 commit。`

// memInputSchema 是 mem 工具的参数表。
func memInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cmd": map[string]any{
				"type":        "string",
				"description": "DSL 命令行，例如 'recall napcat' 或 'note fact -- 端口 1729'",
			},
		},
		"required": []string{"cmd"},
	}
}
