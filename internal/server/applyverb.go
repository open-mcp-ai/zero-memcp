package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// applyPatch 是 apply 动词接收的一条改动。
//
// 它不认识任何业务概念：只有「写这个路径」「删这个路径」两种动作。
// 至于这些路径是 SKILL.md、人格层还是普通流水，服务端一律不知道也不判断。
type applyPatch struct {
	// Op 是 "write" 或 "delete"。
	Op string `json:"op"`
	// Path 是树内相对路径（不带项目/分支前缀）。
	Path string `json:"path"`
	// Content 只在 op=write 时使用。
	Content string `json:"content"`
}

// execApply 批量写。
//
//	apply <project>@<branch> $PATCHES
//
// $PATCHES 是 JSON 数组：
//
//	[
//	  {"op":"write",  "path":"SKILL.md",     "content":"..."},
//	  {"op":"write",  "path":"07-topic.md",  "content":"..."},
//	  {"op":"delete", "path":"08-old.md"}
//	]
//
// # 为什么需要它
//
// 归纳时通常一次改好几个文件（索引 + 若干主题文件）。用 write 就是
// N 次工具往返，把 N 条调用和 N 条返回都塞进模型上下文——而这批改动
// 本来就是一次判断的产物，拆开只是浪费。
//
// # 语义边界
//
// 「先全部校验，再全部落盘」：JSON 解析、路径合法性、权限都在写第一
// 个字节之前完成。所以只要返回成功，整批都写进去了；只要返回错误，
// 一个字节都没写。落盘过程中的磁盘 I/O 失败无法回滚（脏层是追加式
// 的），这种情况会明确报出「已写入前 N 条」，不假装原子。
//
// 不做的事：不自动 commit。写与封箱是两件事，时机归调用方。
func (s *Server) execApply(rest string, c Caller, defaultBranch string) (string, error) {
	addr, payload, ok := strings.Cut(rest, "--")
	if !ok {
		return "", fmt.Errorf(`usage: apply <project>[@<branch>] -- [{"op":"write","path":"...","content":"..."}]`)
	}
	addr = strings.TrimSpace(addr)
	payload = restorePayload(strings.TrimSpace(payload))
	if addr == "" {
		return "", fmt.Errorf("apply needs a branch address")
	}

	proj, branch, path, err := s.resolveAddr(addr, c)
	if err != nil {
		return "", err
	}
	if path != "" {
		return "", fmt.Errorf("%q specifies a file path; apply takes a branch and paths inside the payload", addr)
	}
	_ = defaultBranch
	if !s.allowed(c, proj, branch, configBitWrite) {
		return "", fmt.Errorf("not authorized to write to %s@%s", proj, branch)
	}

	var patches []applyPatch
	if err := json.Unmarshal([]byte(payload), &patches); err != nil {
		return "", fmt.Errorf("payload is not a JSON array of patches: %w", err)
	}
	if len(patches) == 0 {
		return "", fmt.Errorf("payload contains no patches")
	}

	// ── 第一阶段：全部校验，一个字节都不写 ──────────────────────
	seen := make(map[string]bool, len(patches))
	for i, p := range patches {
		op := strings.ToLower(strings.TrimSpace(p.Op))
		if op != "write" && op != "delete" {
			return "", fmt.Errorf("patch #%d: op must be \"write\" or \"delete\", got %q", i+1, p.Op)
		}
		pid := strings.TrimSpace(p.Path)
		if pid == "" {
			return "", fmt.Errorf("patch #%d: path is required", i+1)
		}
		if strings.HasPrefix(pid, "/") || strings.Contains(pid, "..") {
			return "", fmt.Errorf("patch #%d: %q must be a relative path inside the tree", i+1, pid)
		}
		if err := gitstore.ValidateBranch(pid); err != nil {
			return "", fmt.Errorf("patch #%d: invalid path %q: %w", i+1, pid, err)
		}
		if seen[pid] {
			// 同一路径在一次 apply 里出现两次，后者会静默覆盖前者。
			// 那不是调用方想要的——要么合并，要么分两次 apply。
			return "", fmt.Errorf("patch #%d: %q appears more than once in this batch", i+1, pid)
		}
		seen[pid] = true
	}

	repo, err := s.projects.Ensure(proj)
	if err != nil {
		return "", err
	}

	// ── 第二阶段：串行落盘 ─────────────────────────────────────
	var written, deleted int
	for i, p := range patches {
		op := strings.ToLower(strings.TrimSpace(p.Op))
		pid := strings.TrimSpace(p.Path)
		switch op {
		case "write":
			content := []byte(p.Content)
			if err := repo.Write(branch, pid, content, gitstore.ModeContent); err != nil {
				return "", fmt.Errorf("patch #%d/%d (%s): %w — earlier patches in this batch are already applied",
					i+1, len(patches), pid, err)
			}
			written++
		case "delete":
			if err := repo.Delete(branch, pid); err != nil {
				return "", fmt.Errorf("patch #%d/%d (%s): %w — earlier patches in this batch are already applied",
					i+1, len(patches), pid, err)
			}
			deleted++
		}
	}

	// ── 第三阶段：刷新索引 ─────────────────────────────────────
	//
	// 逐个 path 刷（O(路径深度)），不整分支重扫。
	// 索引失败不撤销写入：写入是真的，索引可以重建。
	var refreshErrs []string
	for _, p := range patches {
		pid := strings.TrimSpace(p.Path)
		op := strings.ToLower(strings.TrimSpace(p.Op))
		if err := s.refreshPath(proj, branch, pid, op == "write"); err != nil {
			refreshErrs = append(refreshErrs, fmt.Sprintf("%s: %v", pid, err))
		}
	}

	out := fmt.Sprintf("applied %d change(s) to %s@%s (%d written, %d deleted)",
		len(patches), proj, branch, written, deleted)
	if len(refreshErrs) > 0 {
		out += "\nindex refresh failed for: " + strings.Join(refreshErrs, "; ")
	}
	out += "\nnothing committed — call commit when this batch is ready"
	return out, nil
}
