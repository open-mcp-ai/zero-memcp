package zeromem

// memToolDescription 是给模型看的用法说明。
//
// 写法取舍：不说「这是一个记忆管理系统」那种空话，直接说什么时候用哪个
// 动词。模型读这段话的唯一目的就是决定「我现在该敲哪一行」。
const memToolDescription = `Persistent memory. Writes are queued and immediately readable; commit adds them to history. One line of DSL:

  put [<tree>] -- $TEXT    write memory (pass content through vars/args; do not embed it in cmd)
  put <tree> --ref <addr>  append a reference to existing memory in a tree
  recall <text> [--scope]  semantic search (returns addresses and excerpts)
  read <addr> [<addr>...]  read full text; accepts multiple addresses
  grep <regex> [--scope]   search full text with a regular expression (better than recall for symbols, numbers, and names)
  log [N]                  most recent N entries
  ls [date]                list entries for a date
  trees <project>[@<br>]   view the tree directory
  remind [<tree>]          watch a tree and receive notifications when it changes
  since <tree> <commit>    retrieve tree changes since a commit
  status                   index size

Pass content through vars / args; each reference must occupy a whole word:
  {"cmd": "put team/chat -- $TEXT", "vars": {"TEXT": "content"}}

Multiple branches are the foundation for collaboration across endpoints:
  commit / branch / diff / cp / mv / tag / delete / archive / reset / lock / unlock / batch

When to use each: ask "what did I think about X?" -> recall;
search for exact strings (IP, port, function name) -> grep; ask "what was I working on recently?" -> log.`

// memInputSchema 是 mem 工具的参数表。
func memInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cmd": map[string]any{
				"type":        "string",
				"description": "DSL command line, e.g. 'recall napcat' or 'put -- $TEXT'. Pass content in vars/args and reference it with $NAME / $1.",
			},
			"args": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Positional arguments referenced as $1, $2, etc. in cmd. A reference must occupy a whole word; values are not escaped.",
			},
			"vars": map[string]any{
				"type":        "object",
				"description": "Named arguments referenced as $NAME in cmd. Every supplied value must be referenced.",
			},
		},
		"required": []string{"cmd"},
	}
}
