package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// Caller 是当前请求的调用者身份。
//
// 极其简单：就是「谁在调用」以及「他是不是 root」。
// 权限细节不带在身上——由 access.go 查配置决定。
type Caller struct {
	User string
	Root bool

	// Identity 是这个分身节点的名字（凭证名），例如 "laptop"、"pi-box"。
	// 没给时回落到 User。
	Identity string

	// Mount 是这张凭证的挂载表。nil 表示全命名空间。
	Mount *Mount

	// Cap 是能力上限（rwx 子集），空表示不额外限制。
	Cap string

	// JTI 是这张凭证的编号，写审计用。
	JTI string

	// Session 是调用方声明的会话标识（HTTP 头 X-Memcp-Session）。
	//
	// 它不参与鉴权，只用于事件溯源：资源变更事件带上 origin.session，
	// 客户端据此判断「这次变更是不是我自己这一轮造成的」。没有它，
	// 「自己写的记忆唤醒自己」就只能靠节流硬压，压不准。
	Session string

	// Gated 是这张凭证上被按住、需要临时授权才生效的能力位。
	//
	// 它不是「额外能力」，而是给 cap 已有的位加一道闸门：凭证声明
	// 「我能到 rw」，但要不要现在放它 r 或 w，由 WebUI 逐次决定。
	Gated string
}

// canRead / canWrite / canInject 是 cap 这一层的判定。
//
// 它只是「这张凭证最远能干到哪」，最终还要与 TOML 的权限取交集——
// 所以凭证永远无法自己提权，只能收窄。
func (c Caller) canRead() bool   { return hasCap(c.Cap, 'r') }
func (c Caller) canWrite() bool  { return hasCap(c.Cap, 'w') }
func (c Caller) canInject() bool { return hasCap(c.Cap, 'x') }

// hasCap 判断能力串里有没有某一位。空串表示不限制，全部允许。
func hasCap(cap string, bit rune) bool {
	if cap == "" {
		return true
	}
	return strings.ContainsRune(cap, bit)
}

// authenticate 从请求中解析 Caller。
//
// 顺序：
//  1. Header: Authorization: Bearer <jwt>
//  2. Query:  ?token=<jwt>
//  3. 本机且未带 token：降级为 root（单机开发直连）
func (s *Server) authenticate(r *http.Request) (Caller, error) {
	// 会话标识在入口读一次，所有出口都带上。
	//
	// 只挂在 token 分支上是不够的：本机免签路径（未强制鉴权时降级为
	// root）也照样写记忆、照样推进分支，于是它产生的事件没有 session，
	// 客户端就无法判断「这次变更是不是我自己造成的」——自激循环的
	// 第一道防线直接失效。实测就是这么漏的。
	sess := strings.TrimSpace(r.Header.Get("X-Memcp-Session"))

	token := ""
	if authHdr := r.Header.Get("Authorization"); strings.HasPrefix(authHdr, "Bearer ") {
		token = strings.TrimPrefix(authHdr, "Bearer ")
	}
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	// Basic Auth：git 客户端最常用的方式（用户名随便填，密码是 token）。
	// 不解析用户名——凭证自己带着 sub，用户名写什么都不影响身份。
	if token == "" {
		if u, p, ok := r.BasicAuth(); ok && p != "" {
			_ = u
			token = p
		}
	}

	// 端口策略：整个鉴权过程都要参考它（强制 bearer / 挂载收窄）
	blk, hasBlk := portPolicyFrom(r.Context())

	// HTTP Git 端点一律要 token，没有本机免签。
	//
	// 为什么不给回环地址开例外：git 客户端支持在 URL 里带 token
	// （http://h:p/repos/x.git?token=...），或者用 credential helper
	// 发 Bearer。既然客户端本来就能带，那开例外就只是多一个
	// 「本机上任何进程都能读走全部记忆」的洞——而记忆库恰恰是最不该
	// 让本机任意进程随便读的东西。
	// HTTP Git 与信箱一律要 token，没有本机免签。
	//
	// 为什么信箱不开例外：信箱里躺着的是跨端点投递的未确认消息，
	// 谁都能读就等于破坏了端到端的隔离。
	if strings.HasPrefix(r.URL.Path, "/repos/") && token == "" {
		return Caller{}, fmt.Errorf("this endpoint requires credentials: provide Authorization: Bearer <jwt>")
	}

	if token != "" {
		claims, err := s.tokens.Verify(token)
		if err != nil {
			return Caller{}, fmt.Errorf("invalid credential: %w", err)
		}
		if s.rev.Revoked(claims.JTI) {
			return Caller{}, fmt.Errorf("credential has been revoked (jti=%s)", claims.JTI)
		}
		// 兑换券不能当正式凭证用：它只能去换 token，不能读写记忆。
		if claims.Ticket {
			return Caller{}, fmt.Errorf("this is an exchange ticket (tkt), not valid for read or write; exchange it for a regular credential")
		}

		isRoot := false
		pwd := ""
		if s.cfgIndex() != nil {
			if u, ok := s.cfgIndex().User(claims.Sub); ok {
				isRoot = u.Root
				pwd = u.Pwd
			}
		}

		// 一张凭证就是一个身份：能力与挂载表在签发时定死，
		// 只能被端口进一步收窄，不能由请求头当场改。
		//
		// 早先这里支持 Roles + X-Zero-Role 逐请求切换身份。删掉的理由：
		// 身份是数据，不是鉴权机制。一个数据库连接不会中途换成另一个用户，
		// 想要另一份权限就签另一张凭证。逐请求切换把「我是谁」交给了
		// 传输层，也就把提权入口留给了任何能发 header 的东西。
		capStr := claims.Cap
		mount := claims.Mount

		// 没给 token 级挂载表时，退回配置里该用户的 pwd。
		// 非 root 用户配了 pwd 就被自动关进那个仓库——
		// 这是「受限账户只有自己那一摊」的默认形态。
		if len(mount) == 0 && pwd != "" && !isRoot {
			mount = []auth.MountPoint{{Project: pwd, At: "/"}}
		}

		// 端口级挂载：与凭证自身挂载取交集，只能进一步收窄。
		//
		// 典型用法是「这个端口只暴露某一条分支」——那么即使拿的是
		// 一张全量凭证，从这个端口进来也只能看到那一条。
		//
		// 交集为空必须 fail closed。早先这里直接交给 NewMount，
		// 而空输入被当成 nil（= 全命名空间），于是两个都不包含对方
		// 的挂载表反而把挂载限制整个取消掉——实测凭证只挂 daily、
		// 端口只挂 me 时，能读到第三个项目 other/secret。
		if hasBlk && len(blk.Mount) > 0 {
			narrowed := narrowByPort(mount, blk.Mount)
			if len(narrowed) == 0 {
				return Caller{}, fmt.Errorf("this credential's mount scope does not intersect this port's mount scope; access denied (port %s)", blk.Addr)
			}
			mount = narrowed
		}

		// 节点名：这张凭证是哪个设备/实例在用。它只是审计标签，
		// 不参与任何权限判定。
		nodeName := claims.Name
		if nodeName == "" {
			nodeName = claims.Sub
		}
		if nodeName == "" {
			nodeName = "anonymous"
		}

		return Caller{
			User:     claims.Sub,
			Identity: nodeName,
			Root:     isRoot,
			Mount:    NewMount(mount, s.defaultBranch),
			Cap:      capStr,
			JTI:      claims.JTI,
			// 会话标识由客户端声明，只用于事件溯源，不参与鉴权。
			// 它不在凭证里：会话是易变的，凭证是长期身份。
			Session: sess,
			Gated:    claims.Gated,
		}, nil
	}

	// 端口策略：这个端口要求 bearer 时不接受本机免签降级。
	//
	// 为什么必须显式要求：本机降级是为了单人开发方便，
	// 但一个对外开放的端口如果把 127.0.0.1 当作可信来源，那么
	// 任何能在这台机器上跑进程的东西（包括被攻破的其他服务）
	// 都能拿到 root 权限。端口自己声明 require 才算数。
	if hasBlk && strings.EqualFold(blk.Require, "bearer") {
		return Caller{}, fmt.Errorf("this port requires Authorization: Bearer <jwt>")
	}

	if s.isLocal(r) && !s.cfg.EnforceAuth {
		// 单机未强制鉴权时，降级为配置里的真实 root 用户（全能力、全命名空间）。
		c := s.rootCaller()
		c.Session = sess
		return c, nil
	}

	return Caller{}, fmt.Errorf("authorization credential missing (provide Authorization: Bearer <jwt>)")
}

func (s *Server) isLocal(r *http.Request) bool {
	host := r.RemoteAddr
	return strings.HasPrefix(host, "127.0.0.1:") || strings.HasPrefix(host, "[::1]:") || strings.HasPrefix(host, "localhost:")
}

// execWhoami 实现 whoami 动词。
func (s *Server) execWhoami(c Caller) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("User: %s\n", c.User))
	if c.Root {
		// 「root」不等于「无限制」：挂载表收窄之后，全局管理动词
		// （remote-key / remote-secret 等）会被拒。两句话必须一致，
		// 否则运维拿着一个自称 unrestricted 的凭证去跑全局动词，
		// 报错时不知道该怀疑哪里。判据直接复用 rootUnrestricted，
		// 不另外写一份。
		if s.rootUnrestricted(c) {
			sb.WriteString("Privilege: root (unrestricted global access)\n")
		} else {
			sb.WriteString("Privilege: root (narrowed by mount table; global administration verbs are refused)\n")
			sb.WriteString(fmt.Sprintf("Mount: %s\n", c.Mount.Describe()))
		}
	} else if s.cfgIndex() != nil {
		groups := s.cfgIndex().GroupsOf(c.User)
		if len(groups) > 0 {
			sb.WriteString(fmt.Sprintf("Groups: %s\n", strings.Join(groups, ", ")))
		}
	}
	return strings.TrimSpace(sb.String())
}

// execUsers 列出配置里的用户与组（只读）。
func (s *Server) execUsers() TermResult {
	if s.cfgIndex() == nil {
		return TermResult{Text: "(no config file; running in standalone unrestricted mode)"}
	}
	var sb strings.Builder
	sb.WriteString("USER             ROOT  GROUPS\n")
	for _, u := range s.cfgIndex().Users() {
		root := "no"
		if u.Root {
			root = "yes"
		}
		groups := strings.Join(s.cfgIndex().GroupsOf(u.Name), ", ")
		if groups == "" {
			groups = "-"
		}
		sb.WriteString(fmt.Sprintf("%-16s %-5s %s\n", u.Name, root, groups))
	}
	sb.WriteString("\nPROJECT          OWNER            GROUP            MODE\n")
	for _, p := range s.cfgIndex().Projects() {
		owner := p.Owner
		if owner == "" {
			owner = "-"
		}
		group := p.Group
		if group == "" {
			group = "-"
		}
		sb.WriteString(fmt.Sprintf("%-16s %-16s %-16s %s\n",
			p.Name, owner, group, p.Perm.String()))
	}
	return TermResult{Text: strings.TrimRight(sb.String(), "\n")}
}
