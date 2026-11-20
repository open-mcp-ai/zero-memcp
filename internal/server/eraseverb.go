package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// erase / replace：世界树改写动词。
//
// 这两个动词毁灭数据，所以门槛必须比 delete 高得多：
//
//	delete  只让路径从现在起消失——历史里还在。
//	erase   让一份内容从**整条历史**里消失，重写受影响的所有 commit。
//	replace 同上，但留下占位文本（想保时间线完整时用）。
//
// 为什么按 blob 而不是按路径：在内容寻址存储里同一份内容常在多个
// 路径下、多个 commit 里出现。按路径删只是划掉目录条目，书页还在。
//
// 我们只对自己与自己的 Git 仓库承诺「像从没存在过」：本地松散对象
// 会真的被删掉；进了 pack 的旧对象需要 repack 才能物理清盘，报告里
// 会明说（InPack 字段），不假装已经干净。
//
// 外部托管（GitHub）无法做到这一点——那是所有权问题，不是技术问题。

// execErase 处理 erase 与 replace 共用的解析与应用。
func (s *Server) execErase(rest string, c Caller, defaultBranch string, withReplacement bool) (TermResult, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		if withReplacement {
			return TermResult{}, fmt.Errorf("replace syntax: replace <project>[@<branch>]/<path> $TEXT")
		}
		return TermResult{}, fmt.Errorf("erase syntax: erase <project>[@<branch>]/<path>")
	}

	// replace 的正文用 `-- <内容>` 分隔，与 write 完全一致；
	// payload 变量在 restorePayload 里还原（多行正文必须走 $VAR）。
	var replacement []byte
	addr := rest
	if withReplacement {
		a, content, ok := strings.Cut(rest, "--")
		if !ok {
			return TermResult{}, fmt.Errorf("usage: replace <project>[@<branch>]/<path> -- <content>")
		}
		content = restorePayload(strings.TrimPrefix(content, " "))
		if strings.TrimSpace(content) == "" {
			return TermResult{}, fmt.Errorf("replace needs a payload: the text that takes the old content's place")
		}
		addr = strings.TrimSpace(a)
		replacement = []byte(content)
	}

	// 信封门禁：只有具备 x 能力（改写历史）或 root 的调用方才准执行。
	// 先于任何仓库读取检查，防范能力越权。
	if !strings.Contains(c.Cap, "x") && !c.Root {
		return TermResult{}, fmt.Errorf("erase requires the x capability (rewrite history); your cap is %q", c.Cap)
	}

	project, branch, path, err := s.parseBranchAddr(addr)
	if err != nil {
		return TermResult{}, err
	}
	if !s.branchVisible(c, project, branch) {
		return TermResult{}, fmt.Errorf("%s@%s is outside this credential's visible scope", project, branch)
	}
	if !s.allowed(c, project, branch, configBitWrite) {
		return TermResult{}, fmt.Errorf("%s", s.denyReason(c, project, branch, configBitWrite))
	}

	repo, err := s.projects.Get(project)
	if err != nil {
		return TermResult{}, err
	}

	entry, found, err := repo.Stat(branch, path)
	if err != nil {
		return TermResult{}, err
	}
	if !found {
		return TermResult{}, fmt.Errorf("%s:%s not found on branch %s", project, path, branch)
	}

	rep, err := repo.EraseBlob(branch, entry.Hash, replacement)
	if err != nil {
		return TermResult{}, err
	}

	// 内存索引必须立刻跟上：被抹除的内容不能还留在可搜索的卡片柜里。
	_ = s.refreshPath(project, branch, path, false)

	// 审计：留一条不可回读的标记。
	//
	// 历史可以改写，但「曾经改写过」这件事本身不能消失。
	// 记录里只有位置、时间、操作者与数量，没有内容一字节。
	s.auditErase(c, project, branch, rep)

	var sb strings.Builder
	if withReplacement {
		sb.WriteString(fmt.Sprintf("replaced %s@%s/%s\n", project, branch, path))
	} else {
		sb.WriteString(fmt.Sprintf("erased %s@%s/%s\n", project, branch, path))
	}
	sb.WriteString(fmt.Sprintf("  blob:        %s\n", rep.TargetBlob))
	if rep.ReplacementBlob != "" {
		sb.WriteString(fmt.Sprintf("  replacement: %s\n", rep.ReplacementBlob))
	}
	sb.WriteString(fmt.Sprintf("  head:        %s -> %s\n", shortSHA(rep.OldHead), shortSHA(rep.NewHead)))
	sb.WriteString(fmt.Sprintf("  rewritten:   %d commit(s)\n", rep.RewrittenCommits))
	if len(rep.TouchedPaths) > 0 {
		sb.WriteString(fmt.Sprintf("  paths:       %s\n", strings.Join(rep.TouchedPaths, ", ")))
	}
	sb.WriteString(fmt.Sprintf("  dirty:       %d entr(ies) cleared\n", rep.DirtyCleared))
	if rep.LooseRemoved {
		sb.WriteString("  loose blob:  removed from disk\n")
	}
	if rep.InPack {
		// 必须说出来：物理副本还在 pack 里。
		sb.WriteString("  WARNING: the old blob still exists inside a pack file; run `gc` (repack) to reclaim it physically\n")
	}
	sb.WriteString("  note: history was rewritten; peers must force-sync to converge\n")

	structured := map[string]any{
		"project": project, "branch": branch, "path": path,
		"report": rep,
	}
	return TermResult{Text: strings.TrimRight(sb.String(), "\n"), Structured: structured}, nil
}

// auditErase 把一次销毁写进审计日志（不含内容）。
func (s *Server) auditErase(c Caller, project, branch string, rep gitstore.EraseReport) {
	who := c.Identity
	if who == "" {
		who = c.User
	}
	rec := map[string]any{
		"verb":       "erase",
		"identity":   who,
		"project":    project,
		"branch":     branch,
		"blob":       rep.TargetBlob,
		"old_head":   rep.OldHead,
		"new_head":   rep.NewHead,
		"commits":    rep.RewrittenCommits,
		"paths":      rep.TouchedPaths,
		"at":         rep.At,
		"still_pack": rep.InPack,
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	s.logf("audit: erase %s", string(raw))
}
