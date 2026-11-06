package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// execMerge 处理 merge <project> <branch> [from <remote>]。
//
// 它是对等同步的最后一环：fetch 把远端历史取回本地（不碰本地分支），
// merge 才把远端跟踪引用的内容并进本地分支。
//
// 合并策略由 gitstore.MergeInto 决定（见 merge.go 的注释）：
//   - 快进 / 正交分叉 → 自动完成
//   - 同路径不同内容  → 结构化冲突，拒绝，等人工决议
//
// 权限模型：
//   - 对目标分支需要 w（要推进它的指针）
//   - 对来源分支需要 r（要读它的内容）
//   - 来源只能是远端跟踪引用（refs/remotes/<remote>/<branch>），
//     不能直接合并另一个本地分支——对等同步里「另一台机器」永远以
//     远端跟踪引用的形态存在，本地分支互并是仓库手术，该走 cp/reset。
func (s *Server) execMerge(rest string, c Caller) (TermResult, error) {
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return TermResult{}, fmt.Errorf("merge requires <project> <branch> [from <remote>]")
	}
	project, branch := fields[0], fields[1]
	remote := ""
	if len(fields) >= 4 && fields[2] == "from" {
		remote = fields[3]
	}
	if remote == "" {
		return TermResult{}, fmt.Errorf("merge requires an explicit source: merge %s %s from <remote>", project, branch)
	}

	// 写权限先验：合并会推进目标分支的指针
	if !s.allowed(c, project, branch, configBitWrite) {
		return TermResult{}, fmt.Errorf("merge requires w permission on %s@%s", project, branch)
	}
	// 读权限后验：要读来源分支的内容
	if !s.allowed(c, project, branch, configBitRead) {
		return TermResult{}, fmt.Errorf("merge requires r permission on %s@%s (source content)", project, branch)
	}

	repo, err := s.projects.Get(project)
	if err != nil {
		return TermResult{}, err
	}

	// 来源必须是远端跟踪引用
	src := fmt.Sprintf("refs/remotes/%s/%s", remote, branch)
	if repo.RemoteRef(remote, branch) == "" {
		// 引用不存在：给出可诊断的错误（是这个 remote 没配置，还是没 fetch 过）
		rm, ferr := s.findRemote(project, remote)
		if ferr != nil {
			return TermResult{}, fmt.Errorf("merge source unavailable: %w", ferr)
		}
		return TermResult{}, fmt.Errorf(
			"merge source %s not found; run `fetch %s %s` first", src, project, rm.Name)
	}

	res, err := repo.MergeInto(branch, src)
	if err != nil {
		return TermResult{}, fmt.Errorf("merge %s@%s <- %s: %w", project, branch, remote, err)
	}

	structured := map[string]any{
		"project": project, "branch": branch, "remote": remote, "kind": res.Kind,
	}
	var sb strings.Builder
	switch res.Kind {
	case "up-to-date":
		fmt.Fprintf(&sb, "%s@%s is up to date with %s/%s", project, branch, remote, branch)
	case "ahead":
		fmt.Fprintf(&sb, "%s@%s is ahead of %s/%s; nothing to merge (push instead)", project, branch, remote, branch)
	case "fast-forward":
		fmt.Fprintf(&sb, "%s@%s fast-forwarded to %s\n", project, branch, shortSHA(res.Commit))
		structured["commit"] = res.Commit
	case "merged":
		fmt.Fprintf(&sb, "merged %s/%s into %s@%s as %s\n", remote, branch, project, branch, shortSHA(res.Commit))
		structured["commit"] = res.Commit
		structured["paths"] = res.Added
	case "conflict":
		fmt.Fprintf(&sb, "merge refused: %d path(s) differ between local and %s/%s:\n", len(res.Conflicts), remote, branch)
		for _, p := range res.Conflicts {
			fmt.Fprintf(&sb, "  %s\n", p)
		}
		sb.WriteString("\nconflict is not resolved automatically: the same path holds different\n")
		sb.WriteString("content on each side, which is a decision, not a merge.\n")
		sb.WriteString("resolve by editing the file locally (put/read), then commit and push.\n")
		structured["conflicts"] = res.Conflicts
	}
	return TermResult{Text: strings.TrimRight(sb.String(), "\n"), Structured: structured}, nil
}

// mergeCandidates 列出某个项目里「有远端跟踪引用且与本地分叉」的分支。
// merge 无参数时的建议输出用。
func (s *Server) mergeCandidates(c Caller, project string) []string {
	repo, err := s.projects.Get(project)
	if err != nil {
		return nil
	}
	refs, err := repo.Refs("refs/remotes/")
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, ref := range refs {
		// refs/remotes/<remote>/<branch> -> branch
		rest := strings.TrimPrefix(ref, "refs/remotes/")
		slash := strings.Index(rest, "/")
		if slash <= 0 {
			continue
		}
		branch := rest[slash+1:]
		if seen[branch] {
			continue
		}
		seen[branch] = true
		local, err := repo.Resolve("refs/heads/" + branch)
		if err != nil || local == "" {
			continue
		}
		remoteSHA := repo.RemoteRef(rest[:slash], branch)
		if remoteSHA == "" || remoteSHA == local {
			continue
		}
		if !s.allowed(c, project, branch, configBitRead) {
			continue
		}
		out = append(out, branch)
	}
	sort.Strings(out)
	return out
}

var _ = gitstore.MergeResult{Kind: "merged"}
