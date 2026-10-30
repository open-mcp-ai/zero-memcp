package zeromem

import (
	"net/http"

	"github.com/open-mcp-ai/zero-memcp/internal/server"
)

// Mux 返回把记忆端点挂上去的处理器。
//
// 宿主可以把它整个挂到自己的根路径下（记忆端点自己认 /mcp、/repos/、
// /health 等路径），也可以只挑其中几个。
func (c *Core) Mux() http.Handler { return c.srv.Mux() }

// ReposHandler 只返回 HTTP Git 端点，便于宿主单独装配。
func (c *Core) ReposHandler() http.Handler { return c.srv.ReposMux() }

// ToolDefs 返回全部工具声明（目前只有一个）。
func (c *Core) ToolDefs() []ToolDef { d, _ := c.Tool(); return []ToolDef{d} }

var _ = server.New
