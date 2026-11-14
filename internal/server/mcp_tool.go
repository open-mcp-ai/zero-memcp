package server

import "github.com/open-mcp-ai/termcp/mcp"

// termTool 是 term 工具在 MCP 层的声明。
//
// 形状与 server.go 的 tools/list 分支一致，但用 termcp 的 mcp.Tool 表达：
// 让协议层（tools/list 的字段名、required 的位置、annotations 的省略规则）
// 由 termcp 决定，我们只声明"这个工具叫什么、收什么参数、每个参数什么意思"。
//
// 迁移顺序是先建声明、再比对，比对一致才切默认路径——schema 是客户端可见的
// 表面，改形状等于改契约，不能靠"应该没问题"。
func termTool() mcp.Tool {
	return mcp.NewTool("term",
		mcp.WithDescription("termcp protocol entry point (implemented by this service). Accepts one line of DSL:\n"+
			"  recall <text> [--scope T] semantic search (returns addresses and excerpts)\n"+
			"  read <addr> [<addr>...]  read full text; accepts multiple addresses\n"+
			"  grep [-i] [-c] <regex>   search full text (case-sensitive like shell grep; -i ignores case, -c counts only)\n"+
			"  log [N]                  show dates and summaries of the most recent N entries (default 5)\n"+
			"  ls [date]                list entries for a date\n"+
			"  put [<tree>] -- $TEXT    write content to a memory tree (pass content through vars/args, not in the DSL)\n"+
			"  put <tree> --ref <addr>  append a reference to existing memory in a tree\n"+
			"  trees <project>[@<branch>]  view the tree directory\n"+
			"  remind [<tree>]          watch a tree and receive notifications when it changes\n"+
			"  since <tree> <commit>    retrieve tree changes since a commit\n"+
			"  status                   index size\n"+
			"  help [<verb>]            show usage for a verb; an empty command shows the overview\n"+
			"This is your persistent memory: a successful put is stored locally and remains available after a restart.\n"+
			"When to use each: ask \"what did I think about X?\" -> recall;\n"+
			"search for exact strings (IP, port, QQ number, function name) -> grep;\n"+
			"ask \"what was I working on recently?\" -> log.\n"+
			"If the first word is the name of a downstream MCP, pass the rest through unchanged."),
		mcp.WithString("cmd", mcp.Description("DSL command line, e.g. 'recall napcat' or 'put -- $TEXT'. Pass content in vars/args and reference it with $NAME / $1; a reference must occupy a whole word.")),
		mcp.WithArray("args",
			mcp.Description("Positional arguments referenced as $1, $2, etc. in cmd. A reference must occupy a whole word. Values are not escaped or concatenated; pass content here instead of embedding it in cmd."),
			mcp.Items(map[string]any{"type": "string"})),
		mcp.WithObject("vars",
			mcp.Description("Named arguments referenced as $NAME in cmd. The same rule as args applies: a reference must occupy a whole word, and every supplied value must be referenced."),
			mcp.AdditionalProperties(map[string]any{"type": "string"})),
		mcp.WithBoolean("content",
			mcp.Description("Whether to return a text projection for agents. Set to false for programmatic consumers."),
			mcp.DefaultBool(true)),
		mcp.WithBoolean("structured_content",
			mcp.Description("Whether to return a structured projection for programs. Combine with content:false to return only JSON."),
			mcp.DefaultBool(false)),
		mcp.WithRequired("cmd"),
	)
}
