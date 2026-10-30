package config

import (
	"fmt"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// Check 判定某个用户对某个项目能否做 perm（BitRead/BitWrite/BitExecute）。
//
// 判定顺序：
//  1. 用户是 root → 直接放行
//  2. 显式 [[access]] 规则命中 → 以它为准（覆盖 mode）
//  3. 项目属主命中 → 看 mode 的属主三位
//  4. 用户的某个组命中项目属组 → 看 mode 的属组三位
//  5. 其余 → 看 mode 的其他三位
//
// 为什么严格遵循这个顺序：POSIX 的「属主被否定就不看组位」虽然反直觉，
// 但它排除了「一个人既是属主又在组里，结果到底算哪边」的所有歧义。
// 每一层一旦匹配就确定结果，不往下一层漏。
func Check(l *Lookup, user, project string, perm uint8) bool {
	u, ok := l.User(user)
	if !ok {
		return false
	}

	// 先确认项目存在，再看 root。
	//
	// root 绕过的是权限位，不是存在性。一个未在配置里声明的项目
	// 根本不被服务，连 root 也不该能碰到它——否则「配置文件是真相」
	// 这句话就不成立了。
	p, declared := l.Project(project)
	if !declared {
		return false
	}

	if u.Root {
		return true
	}

	// 显式 [[access]] 例外：给某人开小灶，比 mode 优先。
	if granted, ok := l.AccessFor(user, project); ok {
		return hasPerm(granted, perm)
	}

	if p.Owner != "" && p.Owner == user {
		return p.Perm.Owner&perm == perm
	}

	if p.Group != "" {
		for _, g := range l.GroupsOf(user) {
			if g == p.Group {
				return p.Perm.Group&perm == perm
			}
		}
	}

	return p.Perm.Other&perm == perm
}

// Explain 给出拒绝理由，让错误信息能说清是谁被哪一条挡住了。
func Explain(l *Lookup, user, project string, perm uint8) string {
	u, ok := l.User(user)
	if !ok {
		return fmt.Sprintf("用户 %q 不存在", user)
	}
	p, declared := l.Project(project)
	if !declared {
		return fmt.Sprintf("项目 %q 未在配置里声明", project)
	}
	if u.Root {
		return "root"
	}
	if granted, ok := l.AccessFor(user, project); ok {
		if hasPerm(granted, perm) {
			return "显式授权允许"
		}
		return fmt.Sprintf("显式授权仅允许 %s，缺少 %s", granted, permName(perm))
	}
	switch {
	case p.Owner != "" && p.Owner == user:
		return fmt.Sprintf("属主 %s（项目 %s，%s）缺少 %s 权限",
			user, project, p.Perm.String(), permName(perm))
	default:
		for _, g := range l.GroupsOf(user) {
			if g == p.Group {
				return fmt.Sprintf("属组 %s（项目 %s，%s）缺少 %s 权限",
					g, project, p.Perm.String(), permName(perm))
			}
		}
		return fmt.Sprintf("用户 %s（项目 %s，属主 %q，属组 %q，%s）其他位缺少 %s 权限",
			user, project, p.Owner, p.Group, p.Perm.String(), permName(perm))
	}
}

func hasPerm(granted string, perm uint8) bool {
	switch perm {
	case auth.BitRead:
		return strings.ContainsRune(granted, 'r')
	case auth.BitWrite:
		return strings.ContainsRune(granted, 'w')
	case auth.BitExecute:
		return strings.ContainsRune(granted, 'x')
	}
	return false
}

func permName(perm uint8) string {
	switch perm {
	case auth.BitRead:
		return "r"
	case auth.BitWrite:
		return "w"
	case auth.BitExecute:
		return "x"
	}
	return "?"
}
