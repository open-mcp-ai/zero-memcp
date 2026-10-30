// Package zeromem 是对外暴露的唯一入口。
//
// 为什么要有这一层：internal/ 下的实现会被重构（包会拆、函数会改签名），
// 而那些重构不该让下游跟着改。对外只承诺三件事：
//
//  1. New(Options) 造出一个引擎；
//  2. Core.Tool() 返回一个可注册进任意 MCP 宿主的标准工具；
//  3. Core.Close() 干净退出。
//
// 除此之外的一切都是实现细节。
package zeromem

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/open-mcp-ai/zero-memcp/internal/server"
)

// Options 是启动一个记忆引擎所需的最小信息。
type Options struct {
	// DataDir 是数据根。必填。
	DataDir string

	// ConfigFile 是可选的 TOML 配置。为空时用内置缺省。
	ConfigFile string

	// Endpoint 是本端点标识（local / blackbox / …），写进每条记忆的 frontmatter。
	Endpoint string

	// DefaultProfile 是没指定人格时的默认分支名，缺省 zero。
	DefaultProfile string

	// EnforceAuth 为真时连回环地址也要求 token。
	EnforceAuth bool
}

// Core 是一个运行中的记忆引擎。
//
// 它不监听端口——监听是宿主的事。宿主把 Core.Tool() 挂到自己的
// MCP 服务上，把 Core.ServeHTTP 挂到自己的路由上，两者共享同一个实例。
type Core struct {
	srv *server.Server
}

// New 打开数据目录并构建索引。
//
// 不启动任何监听器：端口归宿主管。这也意味着同一个进程里可以同时
// 跑记忆引擎和别的东西（比如一个终端引擎），各占各的路由。
func New(opts Options) (*Core, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("zeromem: 必须给 DataDir")
	}
	profile := opts.DefaultProfile
	if profile == "" {
		profile = "zero"
	}
	srv, err := server.New(server.Config{
		DataDir:        opts.DataDir,
		ConfigFile:     opts.ConfigFile,
		Endpoint:       opts.Endpoint,
		DefaultProfile: profile,
		EnforceAuth:    opts.EnforceAuth,
	})
	if err != nil {
		return nil, err
	}
	return &Core{srv: srv}, nil
}

// Tool 返回可注册进 MCP 宿主的工具定义与调用函数。
//
// 名字固定为 mem：宿主那边可能还有一个 term（终端），两个工具各管
// 各的动词，不做命名空间合并——合并会让 read/ls/help 这类通用词
// 产生歧义，而歧义要靠前缀消解，最后用户面对的是 mem ls 和 term ls。
// 分成两个工具，schema 自己说明自己，没有消解成本。
func (c *Core) Tool() (ToolDef, ToolFunc) {
	return ToolDef{
		Name:        "mem",
		Description: memToolDescription,
		InputSchema: memInputSchema(),
	}, c.call
}

// ToolDef 是工具声明。字段名与 MCP 的 tools/list 一致。
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolFunc 执行一次调用，返回给调用方的文本投影。
type ToolFunc func(ctx context.Context, args json.RawMessage) (string, error)

// call 执行 mem 工具。
func (c *Core) call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("解析 mem 参数: %w", err)
	}
	return c.srv.ExecLine(ctx, a.Cmd)
}

// Close 停止后台维护并释放资源。
func (c *Core) Close() error { return c.srv.Close() }
