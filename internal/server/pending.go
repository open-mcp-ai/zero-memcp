package server

import (
	"fmt"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// execPending 列出某条分支上尚未提交的改动。
//
//	pending <project>@<branch>
//
// 为什么这是一等动词而不是 status 里的一行摘要：调用方（agent）要
// 把「上次 commit 之后攒下来的这批东西」归纳成一段话，第一步就是
// 拿到清单。没有它就只能 ls 猜或者全量重读整条分支。
//
// 这个动词不认识任何业务概念——它不知道谁是人格层、谁是流水。
// 归纳的策略归调用方，这里只回答「哪些路径还没进 commit」。
func (s *Server) execPending(rest string, c Caller, defaultBranch string) (string, error) {
	addr := strings.TrimSpace(rest)
	if addr == "" {
		return "", fmt.Errorf("usage: pending <project>[@<branch>]")
	}
	proj, branch, path, err := s.resolveAddr(addr, c)
	if err != nil {
		return "", err
	}
	if path != "" {
		return "", fmt.Errorf("%q specifies a file path; pending takes a branch", addr)
	}
	if !s.allowed(c, proj, branch, configBitRead) {
		return "", fmt.Errorf("not authorized to read %s@%s", proj, branch)
	}

	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}
	changes, err := repo.DirtyChanges(branch)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return fmt.Sprintf("%s@%s: nothing pending", proj, branch), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s@%s: %d pending change(s)\n", proj, branch, len(changes))
	for _, ch := range changes {
		mark := "+"
		if ch.Deleted {
			mark = "-"
		}
		// 输出规范地址：调用方拿去 read 时不必再拼项目/分支。
		fmt.Fprintf(&sb, "  %s %s\n", mark, memory.MakeID(proj, branch, ch.Path))
	}
	return sb.String(), nil
}
