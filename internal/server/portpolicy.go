package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// ListenBlock 是一个监听端口及其策略。
//
// 权限落在端口上而不是代码里的 if：一个端口没配写动词，
// 它的路由里根本没有写代码可走；这比任何运行期检查都硬。
type ListenBlock struct {
	Addr    string
	Allow   []string
	Require string
	Mount   []auth.MountPoint
}

// FromConfig 把配置里的端口块翻译成运行期结构。
func FromConfig(blks []config.ListenBlock) []ListenBlock {
	out := make([]ListenBlock, 0, len(blks))
	for _, b := range blks {
		lb := ListenBlock{Addr: b.Addr, Allow: b.Allow, Require: b.Require}
		for _, m := range b.Mount {
			lb.Mount = append(lb.Mount, auth.MountPoint{
				Project: m.Project,
				At:      m.At,
				Branch:  m.Branch,
				Cap:     m.Cap,
			})
		}
		out = append(out, lb)
	}
	return out
}

type portPolicyKey struct{}

func withPortPolicy(ctx context.Context, blk ListenBlock) context.Context {
	return context.WithValue(ctx, portPolicyKey{}, blk)
}

func portPolicyFrom(ctx context.Context) (ListenBlock, bool) {
	blk, ok := ctx.Value(portPolicyKey{}).(ListenBlock)
	return blk, ok
}

// allowedOnPort 报告某个动词在这个端口上是否被允许。
func allowedOnPort(blk ListenBlock, verb string) bool {
	// 没配 allow 视为全量（老配置的单 listen 走这条路）
	if len(blk.Allow) == 0 {
		return true
	}
	for _, a := range blk.Allow {
		if a == "*" || a == verb {
			return true
		}
	}
	return false
}

// portError 生成一条能让人看懂「是哪个端口拦的」的错误。
func portError(blk ListenBlock, verb string) error {
	return fmt.Errorf("动词 %q 没有在这个端口上开启（%s 允许：%s）",
		verb, blk.Addr, strings.Join(blk.Allow, " "))
}

// listenBlocks 从配置文件里取出端口块，没有就用单 listen 兜底。
func listenBlocks(f *config.File) []config.ListenBlock {
	if f == nil {
		return nil
	}
	if len(f.Listens) > 0 {
		return f.Listens
	}
	return nil
}

// narrowByPort 用端口挂载表收窄凭证挂载表。
//
// 规则：端口允许的挂载项，凭证必须先有权访问；两边交集才算数。
// 凭证是空表（全命名空间）时，直接采用端口表——因为端口表本身就是
// 一个更窄的视图，跟「空表代表全部可见」不矛盾。
func narrowByPort(cred, port []auth.MountPoint) []auth.MountPoint {
	if len(cred) == 0 {
		return port
	}
	var out []auth.MountPoint
	for _, p := range port {
		for _, c := range cred {
			if c.Project != p.Project {
				continue
			}
			// 分支：以端口为准；凭证指定了就取交集
			branch := p.Branch
			if branch == "" {
				branch = c.Branch
			} else if c.Branch != "" && c.Branch != p.Branch {
				continue
			}
			capStr := p.Cap
			if capStr == "" {
				capStr = c.Cap
			} else if c.Cap != "" {
				capStr = intersectCap(c.Cap, p.Cap)
			}
			out = append(out, auth.MountPoint{
				Project: p.Project, At: p.At, Branch: branch, Cap: capStr,
			})
		}
	}
	return out
}

// ListenAddrs 返回所有配置的监听地址（用于启动日志）。
func (s *Server) ListenAddrs() []string {
	if len(s.listens) == 0 {
		return []string{s.cfg.Listen}
	}
	out := make([]string, 0, len(s.listens))
	for _, b := range s.listens {
		out = append(out, b.Addr)
	}
	return out
}
