package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/acl"
	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// execACL 处理 acl 动词。它是 agent 通过 MCP 配置实例授权的入口。
//
// 语法：
//
//	acl list
//	acl put <user> <project>[@<branch>] <perm> [--note <说明>]
//	acl put <user> * --admin [--note <说明>]
//	acl rm <user> <project>[@<branch>]
//
// 为什么把「加一条共享分支」做成动词而不是让人改配置文件：
//
// 配置文件是部署事实，改它要 reload，而 reload 会重扫所有仓库；更要命的是
// 它在客户端没有镜像——每台机器各自读自己那份，谁知道哪份是新的。
// 做成动词之后，agent 自己就能在一条 MCP 调用里把权限改完，人也能在
// WebUI 里点同一个后端。两者写的是同一份 acl.json，不会有两个真相。
func (s *Server) execACL(rest string, c Caller) (TermResult, error) {
	if !s.aclActive() {
		return TermResult{}, errors.New("this instance has no ACL store (acl.json could not be opened); permissions fall back to credential mounts")
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return TermResult{}, errors.New("usage: acl list | acl put <user> <project>[@<branch>] <perm> [--note <text>] | acl put <user> * --admin | acl rm <user> <project>[@<branch>]")
	}
	sub, body, _ := strings.Cut(rest, " ")
	sub = strings.TrimSpace(sub)
	body = strings.TrimSpace(body)

	switch sub {
	case "list":
		if err := s.requireRootGlobal(c, 'r'); err != nil {
			return TermResult{}, err
		}
		rules := s.acl.Rules()
		lines := []string{fmt.Sprintf("%d rule(s)", len(rules))}
		for _, r := range rules {
			branch := r.Branch
			if branch == "" {
				branch = "*"
			}
			perm := r.Perm
			if r.Admin {
				perm = strings.TrimSpace(perm + " admin")
			}
			note := ""
			if r.Note != "" {
				note = "  — " + r.Note
			}
			lines = append(lines, fmt.Sprintf("  %-10s %-22s %-14s %s%s",
				r.User, r.Project, branch, perm, note))
		}
		return TermResult{Text: strings.Join(lines, "\n"), Structured: map[string]any{"rules": rules}}, nil

	case "put":
		if err := s.requireRootGlobal(c, 'w'); err != nil {
			return TermResult{}, err
		}
		return s.aclPut(body, c)

	case "rm":
		if err := s.requireRootGlobal(c, 'w'); err != nil {
			return TermResult{}, err
		}
		return s.aclRm(body, c)

	default:
		return TermResult{}, fmt.Errorf("unknown acl subcommand %q (list | put | rm)", sub)
	}
}

// aclPut body 形如：<user> <project>[@<branch>] <perm> [--note ...]
func (s *Server) aclPut(body string, c Caller) (TermResult, error) {
	args, note := splitNoteFlag(body)
	if len(args) < 2 {
		return TermResult{}, errors.New("usage: acl put <user> <project>[@<branch>] <perm> [--note <text>]")
	}
	user := args[0]
	if err := s.validateIdentity(user); err != nil {
		return TermResult{}, err
	}
	target := args[1]

	rule := acl.Rule{User: user, Note: note}

	// admin 形式：acl put <user> * --admin
	admin := strings.Contains(body, "--admin") || target == "*"
	if admin {
		rule.Project = "*"
		rule.Admin = true
		if len(args) >= 3 && !strings.HasPrefix(args[2], "--") {
			rule.Perm = strings.TrimSpace(args[2])
		}
		r, err := s.acl.Put(rule)
		if err != nil {
			return TermResult{}, err
		}
		return TermResult{
			Text:       fmt.Sprintf("admin granted: %s may run administration verbs on this instance now (no re-signing needed)", r.User),
			Structured: map[string]any{"ok": true, "rule": r},
		}, nil
	}

	if len(args) < 3 {
		return TermResult{}, errors.New("usage: acl put <user> <project>[@<branch>] <perm> [--note <text>]")
	}
	perm := strings.TrimSpace(args[2])
	proj, branch, err := s.splitProjectBranch(target)
	if err != nil {
		return TermResult{}, err
	}
	rule.Project = proj
	rule.Branch = branch
	rule.Perm = perm

	if branch != "" && branch != "*" {
		if err := auth.ValidateBranchName(branch); err != nil {
			return TermResult{}, err
		}
	}
	r, err := s.acl.Put(rule)
	if err != nil {
		return TermResult{}, err
	}
	where := r.Project
	if r.Branch == "" {
		where += " (all branches)"
	} else {
		where += "@" + r.Branch
	}
	return TermResult{
		Text:       fmt.Sprintf("granted %s on %s to %s — effective immediately, no credential re-issue needed", r.Perm, where, r.User),
		Structured: map[string]any{"ok": true, "rule": r},
	}, nil
}

func (s *Server) aclRm(body string, c Caller) (TermResult, error) {
	args, _ := splitNoteFlag(body)
	if len(args) < 2 {
		return TermResult{}, errors.New("usage: acl rm <user> <project>[@<branch>]")
	}
	user := args[0]
	if args[1] == "*" {
		// 删 admin 授权
		if err := s.acl.Delete(user, "*", "*"); err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: fmt.Sprintf("admin grant removed for %s", user)}, nil
	}
	proj, branch, err := s.splitProjectBranch(args[1])
	if err != nil {
		return TermResult{}, err
	}
	if err := s.acl.Delete(user, proj, branch); err != nil {
		if errors.Is(err, acl.ErrNotFound) {
			return TermResult{}, fmt.Errorf("no rule for %s on %s", user, args[1])
		}
		return TermResult{}, err
	}
	return TermResult{Text: fmt.Sprintf("removed %s on %s", user, args[1])}, nil
}

// splitNoteFlag 把 "… --note 说明" 拆开。
func splitNoteFlag(body string) ([]string, string) {
	note := ""
	if i := strings.Index(body, "--note"); i >= 0 {
		note = strings.TrimSpace(body[i+len("--note"):])
		body = strings.TrimSpace(body[:i])
	}
	fields := strings.Fields(body)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.HasPrefix(f, "--") {
			continue
		}
		out = append(out, f)
	}
	return out, note
}

// splitProjectBranch 解析 "<project>[@<branch>]"。
//
// "*" 作为分支表示「整条项目的所有分支」，存成空串（acl 包的约定）。
func (s *Server) splitProjectBranch(target string) (string, string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", errors.New("project is required")
	}
	proj, branch, _ := strings.Cut(target, "@")
	proj = strings.Trim(strings.TrimSpace(proj), "/")
	if proj == "" {
		return "", "", errors.New("project is required")
	}
	branch = strings.TrimSpace(branch)
	if branch == "*" {
		branch = ""
	}
	// 项目必须已声明：允许 ACL 指向一个不存在的项目，等于埋一个
	// 「看着能配、实际永远不生效」的坑。
	if s.cfgIndex() != nil && !s.cfgIndex().Declared(proj) {
		return "", "", fmt.Errorf("project %q is not declared in the config", proj)
	}
	return proj, branch, nil
}

// aclRulesForUI 给 WebUI 用的稳定排序副本。
func (s *Server) aclRulesForUI() []acl.Rule {
	if !s.aclActive() {
		return nil
	}
	rules := s.acl.Rules()
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].User != rules[j].User {
			return rules[i].User < rules[j].User
		}
		if rules[i].Project != rules[j].Project {
			return rules[i].Project < rules[j].Project
		}
		return rules[i].Branch < rules[j].Branch
	})
	_ = gitstore.DefaultBranch
	return rules
}

// validateIdentity 确认这个身份真的存在（有一张签发给它的凭证记录）。
//
// 为什么要校验：ACL 规则写错一个身份名，表现是「配了但永远不生效」——
// 没有报错、没有日志，只有下一次权限判定时静默不匹配。这类故障排查起来
// 比崩溃贵得多，所以入口就拦住。
func (s *Server) validateIdentity(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("identity is required (use the credential name, e.g. zero or zero-dev)")
	}
	recs, _ := auth.LoadTokenRecords(s.dataDir)
	for _, rec := range recs {
		if rec.Name == name {
			return nil
		}
	}
	return fmt.Errorf("no credential named %q exists on this instance; ACL rules are keyed by credential identity (claims.name), not by user", name)
}
