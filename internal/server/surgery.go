package server

import (
	"errors"
	"fmt"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// 这一组是「树外科手术」：分叉、打标、跨分支剪切与合并。
//
// 为什么要有它：记忆不是一坨平铺的数据，它天然分层——
// 共识该留在 main，开发细节该在 dev，群聊该在 chat，
// 私密的该切到 private。没有这组动作时，所有东西只能挤在一个
// 分支里，靠路径前缀假装分层，而路径前缀没有权限边界、
// 没有独立历史、也没法整体分叉。
//
// 全部只对 root 开放：它们能改仓库结构，而结构是「谁能看见什么」
// 的物理基础。让普通凭证改结构，等于让它改自己的牢房。
//
// 所有操作都只动 ref 和对象，不删任何 blob。所谓「剪掉」就是把
// 路径从某个分支的树里摘出去，blob 本身仍在对象库里、仍在历史里。

// requireRoot 检查调用方是否是 root。
func (s *Server) requireRoot(c Caller) error {
	if !c.Root {
		return fmt.Errorf("这组动词只对 root 开放（当前身份 %s）", c.User)
	}
	return nil
}

// execBranch 实现 branch 动词。
//
//	branch                              列出全部项目 × 分支
//	branch <project>                    列出某项目的分支
//	branch <project> <new> [from <src>] 新建分支（缺省从默认分支分叉）
func (s *Server) execBranch(rest string, c Caller, persona string) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	args := strings.Fields(rest)

	if len(args) == 0 {
		return s.listBranches("", c), nil
	}
	project := args[0]
	if len(args) == 1 {
		return s.listBranches(project, c), nil
	}

	newBranch := args[1]
	src := persona
	if len(args) >= 4 && strings.EqualFold(args[2], "from") {
		src = args[3]
	}

	if !s.allowed(c, project, src, configBitRead) {
		return "", fmt.Errorf("%s", s.denyReason(c, project, src, configBitRead))
	}
	if !s.allowed(c, project, newBranch, configBitWrite) {
		return "", fmt.Errorf("%s", s.denyReason(c, project, newBranch, configBitWrite))
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		return "", fmt.Errorf("打开项目 %s: %w", project, err)
	}

	sha, err := repo.Resolve(src)
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "", fmt.Errorf("源分支 %q 不存在", src)
	}
	if err := repo.Fork(src, newBranch); err != nil {
		return "", err
	}
	if err := s.Reload(); err != nil {
		return "", err
	}
	return fmt.Sprintf("已从 %s@%s 分叉出 %s@%s（%s）", project, src, project, newBranch, sha[:8]), nil
}

// listBranches 列出分支。
//
// 只写「至少有一条分支可见」的项目：项目标题本身就是信息，
// 先把名字打出来再发现下面一行都没有，等于告诉调用方
// 「这个仓库存在，只是你看不见内容」。
func (s *Server) listBranches(project string, c Caller) string {
	projects := s.declaredProjects()
	if project != "" {
		found := false
		for _, p := range projects {
			if p == project {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("没有 %q 这个项目", project)
		}
		projects = []string{project}
	}

	var sb strings.Builder
	for _, p := range projects {
		repo, err := s.projects.Get(p)
		if err != nil {
			continue
		}
		refs, _ := repo.Refs("refs/heads/")
		if len(refs) == 0 {
			continue
		}
		locked := map[string]bool{}
		for _, name := range repo.LockedBranches() {
			locked[name] = true
		}
		var body strings.Builder
		for _, ref := range refs {
			name := strings.TrimPrefix(ref, "refs/heads/")
			if !s.allowed(c, p, name, configBitRead) {
				continue
			}
			sha, _ := repo.Resolve(name)
			n, _ := repo.List(name)
			when := ""
			if logs, err := repo.Log(name, 1); err == nil && len(logs) > 0 {
				when = logs[0].When.Format("2006-01-02 15:04")
			}
			marked := " "
			if name == s.persona {
				marked = "*"
			}
			lockMark := ""
			if locked[name] {
				lockMark = "  [已锁定]"
			}
			body.WriteString(fmt.Sprintf("  %s %-16s %5d 篇  %s  %s%s\n", marked, name, len(n), when, shortSHA(sha), lockMark))
		}
		if body.Len() == 0 {
			continue
		}
		sb.WriteString(p + "\n")
		sb.WriteString(body.String())
	}
	if sb.Len() == 0 {
		return "没有任何分支"
	}
	sb.WriteString("\n* 表示当前默认人格分支")
	return sb.String()
}

func shortSHA(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// execTag 实现 tag 动词。
//
//	tag                          列出全部 tag
//	tag <project>                列出某项目的 tag
//	tag <project> <name> [<ref>] 打 tag（缺省打在默认分支上）
//	tag -d <project> <name>      删 tag（挪到 refs/tags-removed/，不真删）
func (s *Server) execTag(rest string, c Caller, persona string) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	args := strings.Fields(rest)

	if len(args) == 0 {
		return s.listTags("", c), nil
	}

	if args[0] == "-d" || args[0] == "--delete" {
		if len(args) < 3 {
			return "", fmt.Errorf("用法：tag -d <project> <name>")
		}
		if !s.projectWritableFor(c, args[1]) {
			return "", fmt.Errorf("项目 %q 不在这张凭证的可写范围内", args[1])
		}
		repo, err := s.projects.Get(args[1])
		if err != nil {
			return "", err
		}
		if err := repo.Untag(args[2]); err != nil {
			return "", err
		}
		return fmt.Sprintf("已移除 tag %s@%s（留在 refs/tags-removed/）", args[1], args[2]), nil
	}

	project := args[0]
	if len(args) == 1 {
		return s.listTags(project, c), nil
	}

	name := args[1]
	ref := persona
	if len(args) >= 3 {
		ref = args[2]
	}
	if !s.allowed(c, project, ref, configBitWrite) {
		return "", fmt.Errorf("%s", s.denyReason(c, project, ref, configBitWrite))
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		return "", err
	}
	if err := repo.Tag(name, ref); err != nil {
		return "", err
	}
	sha, _ := repo.Resolve(ref)
	return fmt.Sprintf("已打 tag %s → %s@%s（%s）", name, project, ref, shortSHA(sha)), nil
}

func (s *Server) listTags(project string, c Caller) string {
	projects := s.declaredProjects()
	if project != "" {
		projects = []string{project}
	}
	var sb strings.Builder
	for _, p := range projects {
		if !s.projectVisibleFor(c, p) {
			continue
		}
		repo, err := s.projects.Get(p)
		if err != nil {
			continue
		}
		tags, _ := repo.Tags()
		if len(tags) == 0 {
			continue
		}
		sb.WriteString(p + "\n")
		names := make([]string, 0, len(tags))
		for n := range tags {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			sb.WriteString(fmt.Sprintf("  %-24s %s\n", n, shortSHA(tags[n])))
		}
	}
	if sb.Len() == 0 {
		return "没有任何 tag"
	}
	return sb.String()
}

// execCrossBranch 实现 mv / cp 动词。
//
//	cp <src> -> <dst>
//	mv <src> -> <dst>
//
// 两侧的写法都是 <project>@<branch>/<path>。
// 支持一次搬多个：cp a@main/x.md a@main/y.md -> a@dev/
func (s *Server) execCrossBranch(rest string, c Caller, isMove bool) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	left, right, ok := strings.Cut(rest, "->")
	if !ok {
		return "", fmt.Errorf("用法：%s <project>@<branch>/<path> ... -> <project>@<branch>/[<path>]", map[bool]string{true: "mv", false: "cp"}[isMove])
	}
	srcs := strings.Fields(left)
	if len(srcs) == 0 {
		return "", fmt.Errorf("缺少源地址")
	}
	dst := strings.TrimSpace(right)

	dstProj, dstBranch, dstPath, err := s.parseBranchAddr(dst)
	if err != nil {
		return "", fmt.Errorf("目标地址: %w", err)
	}
	// 目标写成目录（空路径或以 / 结尾）时，保留源的完整相对路径。
	//
	// 为什么保留完整路径而不只是文件名：记忆按 YYYY-MM-DD/ 分目录，
	// 剥掉日期前缀会让不同日期的同名切片撞在一起，而且丢掉了
	// 「这条是哪天的」这个不需要读内容就能知道的信息。
	dstIsDir := dstPath == "" || strings.HasSuffix(dst, "/")

	if !c.Mount.Visible(dstProj, dstBranch) {
		return "", fmt.Errorf("目标 %s@%s 不在挂载表内", dstProj, dstBranch)
	}
	if !s.allowed(c, dstProj, dstBranch, configBitWrite) {
		return "", fmt.Errorf("%s", s.denyReason(c, dstProj, dstBranch, configBitWrite))
	}

	var moved []string
	for _, raw := range srcs {
		srcProj, srcBranch, srcPath, perr := s.parseBranchAddr(raw)
		if perr != nil {
			return "", fmt.Errorf("源地址 %q: %w", raw, perr)
		}
		if !s.allowed(c, srcProj, srcBranch, configBitRead) {
			return "", fmt.Errorf("%s", s.denyReason(c, srcProj, srcBranch, configBitRead))
		}
		// 剪切要求源可写；复制只读源，但目标都必须可写。
		if isMove && !s.allowed(c, srcProj, srcBranch, configBitWrite) {
			return "", fmt.Errorf("%s", s.denyReason(c, srcProj, srcBranch, configBitWrite))
		}
		if srcProj != dstProj {
			return "", fmt.Errorf("暂不支持跨项目搬运（%s → %s）", srcProj, dstProj)
		}

		repo, err := s.projects.Get(srcProj)
		if err != nil {
			return "", err
		}

		target := dstPath
		if dstIsDir {
			target = srcPath
		}

		if isMove {
			// 源和目标路径可以不同：把带日期的切片挪进 quarantine/xxx.md 是常态
			if err := repo.MoveAs(srcBranch, srcPath, dstBranch, target); err != nil {
				return "", err
			}
		} else {
			if err := repo.CopyAs(srcBranch, srcPath, dstBranch, target); err != nil {
				return "", fmt.Errorf("复制 %s@%s/%s: %w", srcProj, srcBranch, srcPath, err)
			}
		}

		// 立刻提交两端，让这次手术当场生效
		if isMove {
			if _, cerr := repo.Commit(srcBranch, fmt.Sprintf("mem: %s 取出 %s", verbName(isMove), srcPath)); cerr != nil {
				return "", fmt.Errorf("提交源分支: %w", cerr)
			}
		}
		if srcBranch != dstBranch {
			if _, cerr := repo.Commit(dstBranch, fmt.Sprintf("mem: %s 收入 %s", verbName(isMove), target)); cerr != nil {
				return "", fmt.Errorf("提交目标分支: %w", cerr)
			}
		}

		// 两端都推进了分支头：各自通知一次，锚定任一侧的人都要知道。
		s.notifyTreeChange(srcProj, srcBranch, "")
		if srcBranch != dstBranch || srcProj != dstProj {
			s.notifyTreeChange(dstProj, dstBranch, "")
		}

		moved = append(moved, fmt.Sprintf("  %s@%s/%s → %s@%s/%s", srcProj, srcBranch, srcPath, dstProj, dstBranch, target))
	}

	if err := s.Reload(); err != nil {
		return "", err
	}
	verb := "复制"
	if isMove {
		verb = "剪切"
	}
	return fmt.Sprintf("%s %d 个文件：\n%s", verb, len(moved), strings.Join(moved, "\n")), nil
}

func verbName(isMove bool) string {
	if isMove {
		return "mv"
	}
	return "cp"
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// modeOf 查出某分支下某路径的模式位。
func modeOf(repo *gitstore.Repo, branch, path string) gitstore.Mode {
	entries, err := repo.List(branch)
	if err != nil {
		return gitstore.ModeContent
	}
	for _, e := range entries {
		if e.Path == path {
			return e.Mode
		}
	}
	return gitstore.ModeContent
}

// parseBranchAddr 解析地址。
//
//	<project>@<branch>/<path>   显式指定分支
//	<project>/<path>            用默认人格分支
//
// 项目名可以含斜杠（blackegg/daily），所以 @ 是唯一可靠的分隔符：
// 项目名里不会有 @，路径里也几乎不会有。在第一个 @ 处切。
func (s *Server) parseBranchAddr(addr string) (project, branch, path string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", "", fmt.Errorf("地址不能为空")
	}

	def := s.persona
	if def == "" {
		def = "zero"
	}

	if at := strings.Index(addr, "@"); at >= 0 {
		project = addr[:at]
		rest := addr[at+1:]
		b, p, ok := strings.Cut(rest, "/")
		branch = b
		if ok {
			path = p
		}
	} else {
		// 没写分支：按已声明项目做最长前缀匹配，剩下的都是路径
		pj, p, ok := memory.SplitAddrWithProjects(addr, s.declaredProjects())
		if !ok {
			return "", "", "", fmt.Errorf("无法从 %q 解析出项目；需要 <project>@<branch>/<path>", addr)
		}
		project = pj
		path = p
		branch = def
	}

	if project == "" {
		return "", "", "", fmt.Errorf("无法从 %q 解析出项目", addr)
	}
	if branch == "" {
		branch = def
	}
	return project, branch, path, nil
}

// execCommit 实现 commit 动词（显式封箱）。
//
//	commit                         提交所有项目的待提交区
//	commit <project>               提交某项目的默认分支
//	commit <project>@<branch>      提交某项目的指定分支
//	commit <project> -m "消息"     带自定义提交信息
func (s *Server) execCommit(rest string, c Caller, persona string) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	rest = strings.TrimSpace(rest)

	msg := ""
	if i := strings.Index(rest, " -m "); i >= 0 {
		msg = strings.Trim(strings.TrimSpace(rest[i+4:]), `"'`)
		rest = strings.TrimSpace(rest[:i])
	}

	// 1. 无参：全部提交
	if rest == "" {
		if !c.Mount.Empty() {
			return "", fmt.Errorf("受限挂载凭证必须指定 commit <project>@<branch>")
		}
		if err := s.requireRoot(c); err != nil {
			return "", err
		}
		n, err := s.projects.CommitAll(persona, "mem: manual commit")
		if err != nil {
			return "", err
		}
		if err := s.Reload(); err != nil {
			return "", err
		}
		return fmt.Sprintf("已提交 %d 个项目的待提交区", n), nil
	}

	// 2. 指定了项目（可能带 @branch）
	proj, branch, _, perr := s.parseBranchAddr(rest)
	if perr != nil {
		return "", perr
	}
	if branch == "" {
		branch = persona
	}
	if !s.allowed(c, proj, branch, configBitWrite) {
		return "", fmt.Errorf("%s", s.denyReason(c, proj, branch, configBitWrite))
	}

	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}
	if repo.Pending(branch) == 0 && len(repo.Deleted(branch)) == 0 {
		return fmt.Sprintf("%s@%s 没有未提交的内容", proj, branch), nil
	}

	count := repo.Pending(branch) + len(repo.Deleted(branch))
	if msg == "" {
		msg = fmt.Sprintf("mem: %s@%s manual commit of %d", proj, branch, count)
	}

	sha, err := repo.Commit(branch, msg)
	if err != nil {
		return "", fmt.Errorf("提交失败: %w", err)
	}
	// 显式提交同样推进了分支头：锚定者必须收到脉冲，
	// 否则「手动封箱」会变成一次无人知晓的静默变更。
	s.lastCommits.Store(treeHeadKey(proj, branch), sha)
	s.notifyTreeChange(proj, branch, "")
	if err := s.Reload(); err != nil {
		return "", err
	}
	return fmt.Sprintf("已提交 %s@%s（%d 条，%s）", proj, branch, count, shortSHA(sha)), nil
}

// execDiff 实现 diff 动词（分支比对 / 未审脏流提取）。
//
//	diff <project>@<branch>                  列出该分支的全部文件（= 和空树比）
//	diff <project>@<from> -> <project>@<to>  列出 to 相对 from 的变化
//
// 审计 Agent 的用法：
//
//	inbox@raw 是脏流，inbox@audited 是上次审完的水位。
//	diff inbox@raw -> inbox@audited 的反向就是「还有哪些没审」。
func (s *Server) execDiff(rest string, c Caller, persona string) (string, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", fmt.Errorf("用法：diff <project>@<branch> 或 diff <project>@<from> -> <project>@<to>")
	}

	var fromAddr, toAddr string
	if left, right, ok := strings.Cut(rest, "->"); ok {
		fromAddr = strings.TrimSpace(left)
		toAddr = strings.TrimSpace(right)
	} else {
		toAddr = rest
	}

	fromProj, fromBranch, _, err := s.parseBranchAddr(fromAddr)
	if err != nil && fromAddr != "" {
		return "", fmt.Errorf("源: %w", err)
	}
	toProj, toBranch, _, err := s.parseBranchAddr(toAddr)
	if err != nil {
		return "", fmt.Errorf("目标: %w", err)
	}
	if fromAddr != "" && fromProj != toProj {
		return "", fmt.Errorf("暂不支持跨项目比对（%s vs %s）", fromProj, toProj)
	}
	if !s.allowed(c, toProj, toBranch, configBitRead) {
		return "", fmt.Errorf("%s", s.denyReason(c, toProj, toBranch, configBitRead))
	}
	if fromAddr != "" && !s.allowed(c, fromProj, fromBranch, configBitRead) {
		return "", fmt.Errorf("%s", s.denyReason(c, fromProj, fromBranch, configBitRead))
	}

	repo, err := s.projects.Get(toProj)
	if err != nil {
		return "", err
	}

	changes, err := repo.Diff(fromBranch, toBranch)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		if fromAddr == "" {
			return fmt.Sprintf("%s@%s 是空的", toProj, toBranch), nil
		}
		return fmt.Sprintf("%s@%s 与 %s@%s 没有差异", toProj, toBranch, fromProj, fromBranch), nil
	}

	var added, modified, deleted []string
	for _, ch := range changes {
		switch ch.Kind {
		case "added":
			added = append(added, ch.Path)
		case "modified":
			modified = append(modified, ch.Path)
		case "deleted":
			deleted = append(deleted, ch.Path)
		}
	}

	var sb strings.Builder
	if fromAddr == "" {
		sb.WriteString(fmt.Sprintf("%s@%s 共 %d 个文件\n", toProj, toBranch, len(changes)))
	} else {
		sb.WriteString(fmt.Sprintf("%s@%s → %s@%s 共 %d 处变化\n", fromProj, fromBranch, toProj, toBranch, len(changes)))
	}
	writeGroup := func(title string, items []string, limit int) {
		if len(items) == 0 {
			return
		}
		sb.WriteString(fmt.Sprintf("\n%s（%d）\n", title, len(items)))
		for i, p := range items {
			if i >= limit {
				sb.WriteString(fmt.Sprintf("  … 还有 %d 个\n", len(items)-limit))
				break
			}
			sb.WriteString("  " + p + "\n")
		}
	}
	writeGroup("新增", added, 40)
	writeGroup("修改", modified, 20)
	writeGroup("删除", deleted, 20)
	return sb.String(), nil
}

// execDelete 实现 delete 动词。
//
//	delete <project>@<branch>/<path> [<path> ...]
//	delete <project>@<branch>/<dir>/          （以 / 结尾表示整棵子树）
//
// 删除走墓碑（gitstore.Delete），与写入共享同一个 commit 边界：
// 它不会立刻改树，而是要等 commit 才生效。这样「删一条」和
// 「写一条」不会各自触发一次提交，把一次整理拆成两次。
func (s *Server) execDelete(rest string, c Caller, persona string) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	args := strings.Fields(rest)
	if len(args) == 0 {
		return "", fmt.Errorf("用法：delete <project>@<branch>/<path> [...]")
	}

	proj, branch, firstPath, err := s.parseBranchAddr(args[0])
	if err != nil {
		return "", err
	}
	if !c.Mount.Visible(proj, branch) {
		return "", fmt.Errorf("%s@%s 不在这张凭证的可见范围内", proj, branch)
	}
	if !s.allowed(c, proj, branch, configBitWrite) {
		return "", errors.New(s.denyReason(c, proj, branch, configBitWrite))
	}

	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}

	// 第一个参数已经解析出项目与分支，后续参数当作同分支下的路径
	paths := []string{firstPath}
	paths = append(paths, args[1:]...)

	var removed []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// 目录（以 / 结尾）展开成该前缀下的全部文件
		if strings.HasSuffix(p, "/") {
			entries, lerr := repo.List(branch)
			if lerr != nil {
				return "", lerr
			}
			hit := 0
			for _, e := range entries {
				if strings.HasPrefix(e.Path, p) {
					if derr := repo.Delete(branch, e.Path); derr != nil {
						return "", derr
					}
					removed = append(removed, e.Path)
					hit++
				}
			}
			if hit == 0 {
				return "", fmt.Errorf("%s 下没有文件（%s@%s）", p, proj, branch)
			}
			continue
		}
		// 先确认它真的存在。
		//
		// repo.Delete 只是标记「这个路径待删」，路径拼错也照样成功——
		// 于是一次手误得到「已标记删除 1 个文件」，而文件还在原地。
		// 删除是少数几个不能沉默的操作。
		if _, rerr := repo.Read(branch, p); rerr != nil {
			return "", fmt.Errorf("%s 不存在（%s@%s）", p, proj, branch)
		}
		if err := repo.Delete(branch, p); err != nil {
			return "", err
		}
		removed = append(removed, p)
	}

	if len(removed) == 0 {
		return "没有任何文件被删除", nil
	}
	// 删除也得重算内存索引，否则被删的切片还能被 recall 搜到
	if err := s.Reload(); err != nil {
		return "", err
	}
	return fmt.Sprintf("已标记删除 %d 个文件（%s@%s，需 commit 才生效）：\n  %s",
		len(removed), proj, branch, strings.Join(removed, "\n  ")), nil
}

// ── 分支生命周期 ───────────────────────────────────────────────

// execArchive 实现 archive 动词：把一个分支的整棵树并入归档分支，然后删除它的 ref。
//
//	archive <project>@<branch> [-> <project>@<archive-as>]
//
// 缺省归档到 <branch>-archived。内容全部保留（复用已有 blob，零拷贝），
// 只是换了条分支。原分支的 ref 会被移走，但按「不真删」的规矩，
// 移到一个不可见的 ref 命名空间 refs/archived/ 下，随时可以还原。
func (s *Server) execArchive(rest string, c Caller) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", fmt.Errorf("用法：archive <project>@<branch> [-> <project>@<as>]")
	}

	srcAddr := rest
	dstAddr := ""
	if l, r, ok := strings.Cut(rest, "->"); ok {
		srcAddr = strings.TrimSpace(l)
		dstAddr = strings.TrimSpace(r)
	}

	proj, branch, _, err := s.parseBranchAddr(srcAddr)
	if err != nil {
		return "", err
	}
	dstBranch := branch + "-archived"
	if dstAddr != "" {
		_, b, _, derr := s.parseBranchAddr(dstAddr)
		if derr != nil {
			return "", derr
		}
		if b != "" {
			dstBranch = b
		}
	}
	if dstBranch == branch {
		return "", fmt.Errorf("归档目标和源分支不能同名")
	}
	if err := s.requireRootAccess(c, proj, branch, configBitWrite); err != nil {
		return "", err
	}
	if dstAddr != "" {
		if err := s.requireRootAccess(c, proj, dstBranch, configBitWrite); err != nil {
			return "", err
		}
	}

	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}
	sha, err := repo.Resolve(branch)
	if err != nil || sha == "" {
		return "", fmt.Errorf("分支 %s@%s 不存在或没有任何提交", proj, branch)
	}

	// 归档 = 把 ref 复制成新分支（零字节，只是写一个 sha）
	if err := repo.Fork(branch, dstBranch); err != nil {
		return "", err
	}
	// 源分支移到不可见命名空间：不删除，只是不再出现在 branch 列表里
	if err := repo.ArchiveRef(branch); err != nil {
		return "", err
	}
	if err := s.Reload(); err != nil {
		return "", err
	}
	return fmt.Sprintf("已归档 %s@%s → %s@%s（%s）；源 ref 移到 refs/archived/%s",
		proj, branch, proj, dstBranch, shortSHA(sha), branch), nil
}

// execReset 实现 reset 动词：丢弃分支的树，重置到另一个分支的当前提交。
//
//	reset <project>@<branch> from <project>@<src>
//
// 用途：脏流分支跑歪了（被投毒、格式错乱、混进无关内容），
// 重置回干净起点比重建便宜。被丢弃的那一版会先自动归档一份，
// 所以「重置」也不是不可逆的。
func (s *Server) execReset(rest string, c Caller) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	rest = strings.TrimSpace(rest)
	left, right, ok := strings.Cut(rest, "from")
	if !ok {
		return "", fmt.Errorf("用法：reset <project>@<branch> from <project>@<src>")
	}
	dstAddr := strings.TrimSpace(left)
	srcAddr := strings.TrimSpace(right)

	dstProj, dstBranch, _, err := s.parseBranchAddr(dstAddr)
	if err != nil {
		return "", err
	}
	srcProj, srcBranch, _, err := s.parseBranchAddr(srcAddr)
	if err != nil {
		return "", err
	}
	if dstProj != srcProj {
		return "", fmt.Errorf("暂不支持跨项目重置")
	}
	if dstBranch == "" || srcBranch == "" {
		return "", fmt.Errorf("两侧都必须指定分支")
	}
	if dstBranch == srcBranch {
		return "", fmt.Errorf("源和目标是同一个分支")
	}
	if err := s.requireRootAccess(c, srcProj, srcBranch, configBitRead); err != nil {
		return "", err
	}
	if err := s.requireRootAccess(c, dstProj, dstBranch, configBitWrite); err != nil {
		return "", err
	}

	repo, err := s.projects.Get(dstProj)
	if err != nil {
		return "", err
	}
	srcSHA, err := repo.Resolve(srcBranch)
	if err != nil || srcSHA == "" {
		return "", fmt.Errorf("源分支 %s@%s 不存在或没有提交", srcProj, srcBranch)
	}

	// 先把当前版本归档一份，再重置——重置不该是单向门
	oldSHA, _ := repo.Resolve(dstBranch)
	if oldSHA != "" {
		stamp := time.Now().Format("20060102-150405")
		if err := repo.Fork(dstBranch, dstBranch+"-reset-"+stamp); err != nil {
			return "", fmt.Errorf("重置前归档失败: %w", err)
		}
	}
	if err := repo.Fork(srcBranch, dstBranch); err != nil {
		return "", err
	}
	if err := s.Reload(); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("已重置 %s@%s → %s@%s（%s）", dstProj, dstBranch, srcProj, srcBranch, shortSHA(srcSHA))
	if oldSHA != "" {
		msg += fmt.Sprintf("；旧版本已归档为 %s-reset-*", dstBranch)
	}
	return msg, nil
}

// execChmod 改一个块的模式位：rule（注入系统提示）或 content（普通内容）。
//
// 为什么需要它：`write` 与 `put` 只能写内容块——规则位写不进去。
// 于是「把一条笔记提升为法则」这件事只能靠外部 git 改 100755，
// 而那要求人先 clone、再 chmod、再 push。规则位决定「什么进系统提示」，
// 它不该是命令行之外能力。
func (s *Server) execChmod(rest string, c Caller, persona string) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return "", fmt.Errorf("用法：chmod <project>[@<branch>]/<path> rule|content")
	}
	addr, want := fields[0], fields[1]
	var mode gitstore.Mode
	switch want {
	case "rule", "x", "755", "100755":
		mode = gitstore.ModeRule
	case "content", "c", "644", "100644":
		mode = gitstore.ModeContent
	default:
		return "", fmt.Errorf("模式只能是 rule 或 content，收到 %q", want)
	}

	proj, branch, path, err := s.resolveAddr(addr, c)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("%q 只给出了项目，没有文件路径", addr)
	}
	if !s.allowed(c, proj, branch, configBitWrite) {
		return "", fmt.Errorf("无权修改 %s@%s", proj, branch)
	}
	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}
	body, err := repo.Read(branch, path)
	if err != nil {
		return "", fmt.Errorf("%s 不存在（%s@%s）", path, proj, branch)
	}
	if err := repo.Write(branch, path, body, mode); err != nil {
		return "", err
	}
	if err := s.Reload(); err != nil {
		return "", fmt.Errorf("已改模式，但重建索引失败：%w", err)
	}
	label := "规则（每轮注入）"
	if mode == gitstore.ModeContent {
		label = "普通内容（不注入）"
	}
	return fmt.Sprintf("%s@%s/%s → %s，需 commit 才生效", proj, branch, path, label), nil
}
