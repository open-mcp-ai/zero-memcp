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
	// WebUI 为真时这个端口注册管理面路由。
	//
	// 管理面与 Agent 通道共用同一个 http.Server，但路由表不同：
	// 没开 WebUI 的端口上，/、/api、/ui/* 根本不存在（404），
	// 而不是「存在但会拒绝」。结构性不存在比判断更可靠。
	WebUI bool
}

// FromConfig 把配置里的端口块翻译成运行期结构。
//
// WebUI 必须存在：在黑盒环境（Docker 容器、npm-wrapper、守护进程）下，
// 用户接触不到终端命令行，WebUI 是完成初始改密、凭证签发与授权的唯一
// 也是必需的通道。
//
// 规则：
//   - 若有任何端口显式配置了 WebUI: true，以显式配置为准（支持端口隔离）；
//   - 若所有端口均未显式声明 WebUI，默认自动在第一个端口（主端口）启用。
//
// 这样高级用户可以把 WebUI 隔在专用回环端口，而默认/极简部署绝不会出现
// 「服务跑起来了但到处 404 无法管理」的死锁。
func FromConfig(blks []config.ListenBlock) []ListenBlock {
	out := make([]ListenBlock, 0, len(blks))
	hasUI := false
	for _, b := range blks {
		if b.WebUI {
			hasUI = true
		}
		lb := ListenBlock{Addr: b.Addr, Allow: b.Allow, Require: b.Require, WebUI: b.WebUI}
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
	if len(out) > 0 && !hasUI {
		out[0].WebUI = true
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

// allowedOnPort reports whether a verb is enabled on this port.
//
// An empty list is "nothing allowed", matching portPolicy.allows. The two used
// to disagree (verbs: everything, git: nothing), so a listener missing `allow`
// silently opened every verb. Config validation now rejects that case, and both
// layers share this one reading of the list.
func allowedOnPort(blk ListenBlock, verb string) bool {
	for _, a := range blk.Allow {
		if a == "*" || a == verb {
			return true
		}
	}
	return false
}

// portError 生成一条能让人看懂「是哪个端口拦的」的错误。
func portError(blk ListenBlock, verb string) error {
	return fmt.Errorf("verb %q is not enabled on this port (%s allowed: %s)",
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
			// 分支取交集："*" 是通配，不是一个真实分支名。
			branch := ""
			switch {
			case p.Branch == "" || p.Branch == "*":
				branch = c.Branch
			case c.Branch == "" || c.Branch == "*":
				branch = p.Branch
			case c.Branch == p.Branch:
				branch = p.Branch
			default:
				continue
			}

			// 虚拟路径也是挂载权限的一部分。必须取两个挂载根的较窄者；
			// 不相交的虚拟子树没有交集，不能把其中一边的根拷贝过来。
			credAt, portAt := normalizeVirtual(c.At), normalizeVirtual(p.At)
			at := ""
			switch {
			case credAt == "/":
				at = p.At
			case portAt == "/":
				at = c.At
			case credAt == portAt:
				at = c.At
			case strings.HasPrefix(credAt, portAt+"/"):
				at = c.At
			case strings.HasPrefix(portAt, credAt+"/"):
				at = p.At
			default:
				continue
			}

			capStr := p.Cap
			if capStr == "" {
				capStr = c.Cap
			} else if c.Cap != "" {
				capStr = intersectCap(c.Cap, p.Cap)
			}
			out = append(out, auth.MountPoint{
				Project: p.Project, At: at, Branch: branch, Cap: capStr,
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
