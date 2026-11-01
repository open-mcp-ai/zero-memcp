package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// Caller 是当前请求的调用者身份。
//
// 极其简单：就是「谁在调用」以及「他是不是 root」。
// 权限细节不带在身上——由 access.go 查配置决定。
// rootRoleName 是唯一能绕过挂载表的角色名。
//
// 它不是一个特权开关，而是「这个角色的挂载表本来就覆盖全部项目」
// 的显式标记——用名字表达意图，比用能力位推断更难出错。
const rootRoleName = "zero"

type Caller struct {
	User string
	Root bool

	// Identity 是这个分身节点的名字（凭证名），例如 "laptop"、"pi-box"。
	// 没给时回落到 Role 或 User。
	Identity string

	// Mount 是这张凭证的挂载表。nil 表示全命名空间。
	Mount *Mount

	// Cap 是能力上限（rwx 子集），空表示不额外限制。
	Cap string

	// JTI 是这张凭证的编号，写审计用。
	JTI string

	// Role 是本次请求选中的角色名（可能为空）。
	Role string
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
		return Caller{}, fmt.Errorf("该端点需要凭证：请带 Authorization: Bearer <jwt>")
	}

	if token != "" {
		claims, err := s.tokens.Verify(token)
		if err != nil {
			return Caller{}, fmt.Errorf("无效凭证: %w", err)
		}
		if s.rev.Revoked(claims.JTI) {
			return Caller{}, fmt.Errorf("凭证已被吊销 (jti=%s)", claims.JTI)
		}
		// 兑换券不能当正式凭证用：它只能去换 token，不能读写记忆。
		if claims.Ticket {
			return Caller{}, fmt.Errorf("这是一张兑换券（tkt），不能直接用于读写；请用它换取正式凭证")
		}

		isRoot := false
		pwd := ""
		if s.cfgIndex() != nil {
			if u, ok := s.cfgIndex().User(claims.Sub); ok {
				isRoot = u.Root
				pwd = u.Pwd
			}
		}

		// ── 角色选择 ──
		//
		// 有 Roles 时，能力与挂载表都来自「本次选中的角色」。
		// 角色名只从 header 取：那是客户端/用户的地盘（pi 的 /mem:role 命令），
		// 模型碰不到。把它做成工具参数等于把提权写进协议。
		capStr := claims.Cap
		mount := claims.Mount

		if len(claims.Roles) > 0 {
			roleName := r.Header.Get("X-Zero-Role")
			if roleName == "" {
				roleName = claims.Default
			}
			if roleName == "" {
				return Caller{}, fmt.Errorf("这张凭证定义了角色，但请求没指定（X-Zero-Role），也没有默认角色")
			}
			role, ok := claims.Roles[roleName]
			if !ok {
				return Caller{}, fmt.Errorf("未知角色 %q（可用：%s）", roleName, strings.Join(roleNames(claims.Roles), " "))
			}
			capStr = role.Cap
			mount = role.Mount
			// 角色存在时，只有名为 "zero" 的角色保留 root。
			//
			// 不能靠能力位推断（曾经试过 hasFullCap）：chat 角色
			// 为了拿到 x（规则注入）也会声明 cap=rwx，于是被误判成
			// 全能力角色、继续保留 root，挂载表又一次被短路——
			// 权限隔离看着做了，实际没生效。
			//
			// 名字是显式意图，能力位是本事。判断「谁能绕过挂载表」
			// 必须用意图，不能用本事。
			isRoot = isRoot && roleName == rootRoleName
		}

		// 没给 token 级挂载表时，退回配置里该用户的 pwd。
		// 非 root 用户配了 pwd 就被自动关进那个仓库——
		// 这是「受限账户只有自己那一摊」的默认形态。
		if len(mount) == 0 && pwd != "" && !isRoot && len(claims.Roles) == 0 {
			mount = []auth.MountPoint{{Project: pwd, At: "/"}}
		}

		effectiveRole := r.Header.Get("X-Zero-Role")
		if effectiveRole == "" && len(claims.Roles) > 0 {
			effectiveRole = claims.Default
		}
		// 端口级挂载：与凭证自身挂载取交集，只能进一步收窄。
		//
		// 典型用法是「这个端口只暴露 chat 分支」——那么即使拿的是
		// 一张全量凭证，从这个端口进来也只能看到 chat。
		if hasBlk && len(blk.Mount) > 0 {
			mount = narrowByPort(mount, blk.Mount)
		}

		nodeName := claims.Name
		if nodeName == "" {
			nodeName = effectiveRole
		}
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
			Mount:    NewMount(mount, s.persona),
			Cap:      capStr,
			JTI:      claims.JTI,
			Role:     effectiveRole,
		}, nil
	}

	// 端口策略：这个端口要求 bearer 时不接受本机免签降级。
	//
	// 为什么必须显式要求：本机降级是为了单人开发方便，
	// 但一个对外开放的端口如果把 127.0.0.1 当作可信来源，那么
	// 任何能在这台机器上跑进程的东西（包括被攻破的其他服务）
	// 都能拿到 root 权限。端口自己声明 require 才算数。
	if hasBlk && strings.EqualFold(blk.Require, "bearer") {
		return Caller{}, fmt.Errorf("这个端口要求 Authorization: Bearer <jwt>")
	}

	if s.isLocal(r) && !s.cfg.EnforceAuth {
		// 单机未强制鉴权时，降级为配置里的真实 root 用户（全能力、全命名空间）
		return s.rootCaller(), nil
	}

	return Caller{}, fmt.Errorf("缺少授权凭证 (请提供 Authorization: Bearer <jwt>)")
}

func (s *Server) isLocal(r *http.Request) bool {
	host := r.RemoteAddr
	return strings.HasPrefix(host, "127.0.0.1:") || strings.HasPrefix(host, "[::1]:") || strings.HasPrefix(host, "localhost:")
}

// execWhoami 实现 whoami 动词。
func (s *Server) execWhoami(c Caller) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("User: %s\n", c.User))
	if c.Role != "" {
		sb.WriteString(fmt.Sprintf("Role: %s\n", c.Role))
	}
	if c.Root {
		sb.WriteString("Privilege: root (拥有全局直通权限)\n")
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
		return TermResult{Text: "（无配置文件，当前为单机全放行模式）"}
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

// roleNames 列出全部角色名，按字典序，供错误信息用。
func roleNames(roles map[string]auth.Role) []string {
	out := make([]string, 0, len(roles))
	for k := range roles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
