package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/acl"
)

// 收窄档案（role）的服务端部分：动词、绑定校验、以及收窄判定。
//
// 不变式只有一条，整个机制都围着它转：
//
//	**role 只能让权限从 true 变成 false，永远不能反过来。**
//
// 所以即使请求里的 role 是被提示词注入骗着自报的，攻击者得到的也只是
// 「自己把自己关窄」——凭证本来没有的东西，一个字段变不出来。

// roleAllows 报告本次调用声明的 role 是否允许 (project, branch, perm)。
//
// 无 role、role 表不可用、或该分支不在 role 的任何边界里时：
// 前两者不额外限制（真正的边界由 tokenCeiling 守着），
// 最后一种**拒绝**——「role 里没写到」等于「不允许」，因为一个叫
// 「只看贴吧」的档案如果对群聊静默放行，它就不是收窄而是装饰。
func (s *Server) roleAllows(c Caller, project, branch string, perm uint8) bool {
	return s.roleAllowsPath(c, project, branch, "", perm)
}

// roleAllowsPath 是带子树的版本：path 为空表示只判分支级。
func (s *Server) roleAllowsPath(c Caller, project, branch, path string, perm uint8) bool {
	if c.Role == "" {
		return true
	}
	role, ok := s.activeRole(c)
	if !ok {
		// 声明了一个不存在的 role：拒绝。
		//
		// 静默放行是最坏的选择——调用方以为自己收窄了，实际没有，
		// 而它之后的每一次判断都建立在这个错误前提上。
		return false
	}
	return role.Allows(project, branch, path, perm)
}

// activeRole 取出本次调用可用的 role。
//
// 三条判据，缺一不可：
//  1. role 表可用（否则没东西可查）；
//  2. role 存在；
//  3. 该 role 已绑定给这张凭证的身份（claims.name）。
//
// 第 3 条不是安全必需（收窄不会提权），而是正确性：绑定让「谁能用这个
// 档案」成为一件可审计的事，也让「拼错 role 名」当场报错，而不是
// 变成一个悄悄不生效的限制。
func (s *Server) activeRole(c Caller) (acl.Role, bool) {
	if s.roles == nil || c.Role == "" {
		return acl.Role{}, false
	}
	role, ok := s.roles.Lookup(c.Role)
	if !ok {
		return acl.Role{}, false
	}
	for _, id := range role.BoundTo {
		if id == c.Identity {
			return role, true
		}
	}
	return acl.Role{}, false
}

// roleError 给出「这个 role 为什么不可用」的明确理由。
func (s *Server) roleError(c Caller) error {
	if c.Role == "" {
		return nil
	}
	if s.roles == nil {
		return invalidf(map[string]any{"role": c.Role, "reason": "roles_unavailable"},
			"role %q cannot be used: this instance has no usable role store (system/roles.json)", c.Role)
	}
	role, ok := s.roles.Lookup(c.Role)
	if !ok {
		return invalidf(map[string]any{"role": c.Role, "reason": "unknown_role"},
			"no role named %q (see `role ls`)", c.Role)
	}
	for _, id := range role.BoundTo {
		if id == c.Identity {
			return nil
		}
	}
	return forbiddenf(map[string]any{"role": c.Role, "identity": c.Identity, "reason": "role_not_bound"},
		"role %q is not bound to this credential (%s); an admin can bind it with: role bind %s %s",
		c.Role, c.Identity, c.Role, c.Identity)
}

// execRole 实现 role 动词（root only 的写操作）。
//
//	role ls
//	role put <name> <proj>[@<br>[:<sub>]] <perm> [<spec> <perm> ...] [--note <text>]
//	role bind <name> <identity> [<identity> ...]
//	role unbind <name>
//	role rm <name>
func (s *Server) execRole(rest string, c Caller) (TermResult, error) {
	if s.roles == nil {
		return TermResult{}, errors.New("this instance has no usable role store (system/roles.json unreadable)")
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return TermResult{}, errors.New("usage: role ls | role put <name> <spec> <perm> [...] | role bind <name> <identity>... | role unbind <name> | role rm <name>")
	}
	sub, body, _ := strings.Cut(rest, " ")
	sub = strings.TrimSpace(sub)
	body = strings.TrimSpace(body)

	switch sub {
	case "ls", "list":
		if err := s.requireRootGlobal(c, 'r'); err != nil {
			return TermResult{}, err
		}
		roles := s.roles.Roles()
		lines := []string{fmt.Sprintf("%d role(s)", len(roles))}
		for _, r := range roles {
			bound := "-"
			if len(r.BoundTo) > 0 {
				bound = strings.Join(r.BoundTo, ",")
			}
			note := ""
			if r.Note != "" {
				note = "  — " + r.Note
			}
			lines = append(lines, fmt.Sprintf("  %-16s bound=%s%s", r.Name, bound, note))
			for _, e := range r.Entries {
				branch := e.Branch
				if branch == "" {
					branch = "*"
				}
				path := ""
				if e.Path != "" {
					path = ":" + e.Path
				}
				lines = append(lines, fmt.Sprintf("      %s@%s%s %s", e.Project, branch, path, e.Perm))
			}
		}
		return TermResult{Text: strings.Join(lines, "\n"), Structured: map[string]any{"roles": roles}}, nil

	case "put":
		if err := s.requireRootGlobal(c, 'w'); err != nil {
			return TermResult{}, err
		}
		return s.rolePut(body, c)

	case "bind":
		if err := s.requireRootGlobal(c, 'w'); err != nil {
			return TermResult{}, err
		}
		return s.roleBind(body)

	case "unbind":
		if err := s.requireRootGlobal(c, 'w'); err != nil {
			return TermResult{}, err
		}
		name := strings.TrimSpace(body)
		if name == "" {
			return TermResult{}, errors.New("usage: role unbind <name>")
		}
		r, err := s.roles.Bind(name, nil)
		if err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: fmt.Sprintf("role %s unbound (no identity may use it now)", r.Name)}, nil

	case "rm", "delete":
		if err := s.requireRootGlobal(c, 'w'); err != nil {
			return TermResult{}, err
		}
		name := strings.TrimSpace(body)
		if name == "" {
			return TermResult{}, errors.New("usage: role rm <name>")
		}
		if err := s.roles.Delete(name); err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: fmt.Sprintf("role %s deleted", name)}, nil

	default:
		return TermResult{}, invalidf(nil, "unknown role subcommand %q (ls | put | bind | unbind | rm)", sub)
	}
}

// rolePut 定义/替换一个 role。
//
// 语法的选择很克制：`<name> <spec> <perm>` 成对出现，不引入嵌套结构。
//
//	role put tieba "BlackEggAI/zero@main.record:record/贴吧" r
//	role put tieba "BlackEggAI/zero@main.record:record/贴吧" r "BlackEggAI/zero@main.fact" r
func (s *Server) rolePut(body string, c Caller) (TermResult, error) {
	fields := strings.Fields(body)
	if len(fields) < 3 {
		return TermResult{}, errors.New("usage: role put <name> <project>[@<branch>[:<subtree>]] <perm> [<spec> <perm> ...] [--note <text>]")
	}
	name := fields[0]
	if err := acl.ValidateRoleName(name); err != nil {
		return TermResult{}, err
	}
	// --note 取到行尾。
	note := ""
	args := fields[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--note" {
			note = strings.Join(args[i+1:], " ")
			args = args[:i]
			break
		}
	}
	if len(args) == 0 || len(args)%2 != 0 {
		return TermResult{}, errors.New("role put: entries must come in <spec> <perm> pairs")
	}
	var entries []acl.RoleEntry
	for i := 0; i < len(args); i += 2 {
		spec, err := acl.ParseRoleSpec(args[i])
		if err != nil {
			return TermResult{}, err
		}
		perm := args[i+1]
		for _, ch := range perm {
			if !strings.ContainsRune("rwx", ch) {
				return TermResult{}, invalidf(nil, "role put: invalid perm %q (allowed letters: rwx)", perm)
			}
		}
		spec.Perm = perm
		entries = append(entries, spec)
	}

	// 天花板校验：role 的每一条都必须在**管理员自己的**凭证范围之内。
	//
	// 用管理员的天花板而不是某个被绑身份的天花板：role 是给多个身份共享的
	// 档案，定义时按「谁能定义它」来限制才说得通（定义者是 root）。
	// 真正生效时的收紧由调用侧再与各自的 token 取交集完成——两层都做，
	// 因为任何一层单独做都不够：定义时只防手滑，调用时才防越权。
	for _, e := range entries {
		if !s.rootUnrestricted(c) {
			// 非不受限 root 的 admin 只能在自己看得见的范围内定义。
			perm := permBitOf(e.Perm)
			if perm == 0 || !s.tokenCeiling(c, e.Project, chooseBranch(e.Branch, s.defaultBranch), perm) {
				return TermResult{}, forbiddenf(map[string]any{"spec": e.Project + "@" + e.Branch},
					"role %q: entry %s@%s %s exceeds your own credential's ceiling",
					name, e.Project, e.Branch, e.Perm)
			}
		}
	}

	// 保留已有的绑定（重新定义边界不该顺手把谁能用丢了）。
	prev, hadPrev := s.roles.Lookup(name)
	role := acl.Role{Name: name, Entries: entries, Note: note}
	if hadPrev {
		role.BoundTo = prev.BoundTo
		role.UpdatedAt = prev.UpdatedAt
	}
	if err := s.roles.Put(role); err != nil {
		return TermResult{}, err
	}
	saved, _ := s.roles.Lookup(name)
	return TermResult{
		Text: fmt.Sprintf("role %s saved: %d entr%s, bound to %v",
			name, len(entries), plural(len(entries)), saved.BoundTo),
		Structured: map[string]any{"role": saved},
	}, nil
}

func (s *Server) roleBind(body string) (TermResult, error) {
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return TermResult{}, errors.New("usage: role bind <name> <identity> [<identity> ...]")
	}
	name := fields[0]
	ids := append([]string(nil), fields[1:]...)
	// 身份必须是真实存在的凭证（与 acl put 同一条纪律）：
	// 绑给一个不存在的身份，等于写了一条永远不会生效的规则。
	for _, id := range ids {
		if err := s.validateIdentity(id); err != nil {
			return TermResult{}, err
		}
	}
	r, err := s.roles.Bind(name, ids)
	if err != nil {
		return TermResult{}, err
	}
	sort.Strings(r.BoundTo)
	return TermResult{
		Text:       fmt.Sprintf("role %s bound to %v", name, r.BoundTo),
		Structured: map[string]any{"role": r},
	}, nil
}

// roleNoteForRequest 把「本次调用声明了 role」这件事做成一行可审计的说明。
//
// 只在显式声明时返回内容：绝大多数调用没有 role，不该多一行噪声。
func roleNoteForRequest(c Caller) string {
	if c.Role == "" {
		return ""
	}
	return "role: " + c.Role
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func chooseBranch(branch, def string) string {
	if branch == "" || branch == "*" {
		return def
	}
	return branch
}

// permBitOf 把权限字母串折算成一个权限位（用于天花板校验）。
//
// 一位一位查而不是只看第一个字母：`rw` 里只要有一位越界，就必须整条拒绝，
// 否则「rw」在只读凭证下会被当成合法的「r」。
func permBitOf(perm string) uint8 {
	if perm == "" {
		return 0
	}
	var bit uint8
	for _, ch := range perm {
		switch ch {
		case 'r':
			bit = configBitRead
		case 'w':
			bit = configBitWrite
		case 'x':
			bit = configBitExecute
		default:
			return 0
		}
	}
	return bit
}

// roleDenyError 生成「被收窄档案拒绝」的明确错误。
//
// 必须说清是 role 挡的、以及它允许什么：否则调用方会以为自己没权限、
// 去改 ACL 或换凭证——而真相只是它声明了一个更窄的档案。
func (s *Server) roleDenyError(c Caller, project, branch, path string) error {
	role, ok := s.activeRole(c)
	allowed := "(role store unavailable)"
	if ok {
		var parts []string
		for _, e := range role.Entries {
			b := e.Branch
			if b == "" {
				b = "*"
			}
			p := ""
			if e.Path != "" {
				p = ":" + e.Path
			}
			parts = append(parts, e.Project+"@"+b+p+" "+e.Perm)
		}
		allowed = strings.Join(parts, ", ")
		if allowed == "" {
			allowed = "(denies everything)"
		}
	}
	return forbiddenf(map[string]any{
		"role": c.Role, "project": project, "branch": branch, "path": path, "reason": "outside_role_scope",
	}, "%s@%s/%s is outside role %q (this role allows: %s)", project, branch, path, c.Role, allowed)
}
