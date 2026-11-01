package server

import (
	"errors"
	"fmt"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// put 是唯一的写入原语：写记忆、写想法、写引用，都是它。
//
// 三种写法：
//
//	put -- $TEXT                默认流水归宿，按日期自动分片
//	put <tree> -- $TEXT         写进指定的树路径（可带 @branch）
//	put <tree> --ref <addr>     引用一条已有切片，不复制正文
//
// 目标即类型：落进记忆树的正文是记忆，落进某棵子树的想法是想法。
// 不再有一个 kind 参数要求调用方先给自己分类。
//
// 写入后立刻提交该分支：这样树 SHA 前进，锚定在这棵树上的
// 关注者才收得到变更脉冲。攒批提交会把通知延迟到不确定的时刻，
// 而「提醒」的价值全在及时。

// execPut 是 put 动词的入口。
func (s *Server) execPut(rest string, c Caller) (TermResult, error) {
	if !c.canWrite() {
		return TermResult{}, fmt.Errorf("凭证缺少 w 能力")
	}

	head, body, hasBody := strings.Cut(rest, "--")
	head = strings.TrimSpace(head)
	body = restorePayload(strings.TrimSpace(body))

	// --ref <addr> 可以出现在头部（put tree --ref addr 会被 Cut 成 head="tree "）。
	isRef := false
	refAddr := ""
	if fields := strings.Fields(head); len(fields) >= 2 && fields[len(fields)-2] == "--ref" {
		isRef = true
		refAddr = fields[len(fields)-1]
		head = strings.TrimSpace(strings.Join(fields[:len(fields)-2], " "))
		body = ""
	} else if strings.HasPrefix(body, "ref ") || strings.HasPrefix(body, "--ref ") {
		isRef = true
		body = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(body, "--"), "ref"))
		refAddr = strings.TrimSpace(body)
		body = ""
	}

	if isRef {
		if refAddr == "" {
			return TermResult{}, errors.New("put --ref 需要一个地址")
		}
	} else {
		if !hasBody {
			return TermResult{}, errors.New("put 语法: put [<tree>] -- $TEXT 或 put <tree> --ref <addr>")
		}
		if body == "" {
			return TermResult{}, errors.New("正文不能为空")
		}
	}

	// 头部里允许一个 @session，用来在面板里按会话分组。
	session := ""
	kept := make([]string, 0, 2)
	for _, tok := range strings.Fields(head) {
		if strings.HasPrefix(tok, "@") && !strings.Contains(tok, "/") {
			session = strings.TrimPrefix(tok, "@")
			continue
		}
		kept = append(kept, tok)
	}
	target := strings.Join(kept, " ")

	if isRef {
		return s.putRef(target, refAddr, session, c)
	}
	return s.putBody(target, body, session, c)
}

// putSpec 是把 put 的目标解析成写入位置的结果。
type putSpec struct {
	// Explicit 是传给写入层的目标（"project" 或 "project@branch"）；空表示由流水策略决定。
	Explicit string
	// TreePath 是树内前缀；正文会落在这棵子树下面。
	TreePath string
}

// parsePutSpec 把 "proj@branch/path"、"proj/path"、"path" 三种写法归一。
//
// 不猜：项目名必须是已声明的前缀，否则整串当作树路径。
// 猜错的后果是内容落进另一个仓库，而且不报错——这类失败最难查。
func (s *Server) parsePutSpec(target string, c Caller) putSpec {
	target = strings.Trim(strings.TrimSpace(target), "/")
	if target == "" {
		return putSpec{}
	}

	// 显式分支：proj@branch[/path]
	if at := strings.Index(target, "@"); at >= 0 {
		proj := target[:at]
		rest := target[at+1:]
		br, p, _ := strings.Cut(rest, "/")
		return putSpec{Explicit: proj + "@" + br, TreePath: p}
	}

	// 已声明项目前缀：proj[/path]
	for _, p := range s.declaredProjects() {
		if target == p {
			return putSpec{Explicit: p}
		}
		if strings.HasPrefix(target, p+"/") {
			return putSpec{Explicit: p, TreePath: strings.TrimPrefix(target, p+"/")}
		}
	}

	// 挂载表下的虚拟路径
	if !c.Mount.Empty() {
		if proj, br, sub, err := c.Mount.ToReal(target); err == nil && proj != "" {
			spec := putSpec{Explicit: proj + "@" + br}
			spec.TreePath = strings.Trim(sub, "/")
			if spec.TreePath == "" {
				spec.TreePath = target
			}
			return spec
		}
	}

	// 其余情况：当作默认写入项目下的一棵子树。
	return putSpec{TreePath: target}
}

// putBody 写入一条正文。
func (s *Server) putBody(target, body, session string, c Caller) (TermResult, error) {
	spec := s.parsePutSpec(target, c)

	source := c.Role
	if source == "" {
		source = "mcp"
	}

	path, proj, branch, err := s.writeNoteGit("", body, session, source, c, spec.Explicit, spec.TreePath)
	if err != nil {
		return TermResult{}, err
	}
	rel := strings.TrimPrefix(path, proj+"/")

	// 不提交：put 只承诺「已投递进脏层」。
	//
	// 什么时候把脏层折叠成 Git 历史，是策略的事（后台攒批或显式 commit），
	// 不是单次写入的事。写成即提交会让历史碎成千万个单条 commit，
	// 也会把审计/分批审阅的边界打乱。
	//
	// repo.Write 返回成功 = 已进入单写者队列；队列满会同步报错，
	// 所以走到这里的一定是被采纳的写入。worker 后续的 I/O 失败会由
	// 下一次 barrier（read/recall/commit/status）带出来，不静默吞掉。
	repo, err := s.projects.Get(proj)
	if err != nil {
		return TermResult{}, err
	}
	commitSHA, _ := repo.Resolve(branch)
	virtPath := c.Mount.ToVirtual(proj, branch, rel)

	text := "ok " + virtPath
	if commitSHA != "" {
		text += " (base " + shortSHA(commitSHA) + ")"
	}
	return TermResult{
		Text: text,
		Structured: map[string]any{
			"ok":      true,
			"queued":  true,
			"pending": repo.Pending(branch),
			"path":    virtPath,
			"tree":    spec.TreePath,
			"project": proj,
			"branch":  branch,
			"base":    commitSHA,
			"clock":   s.Clock(),
		},
	}, nil
}

// putRef 追加一条指向已有切片的引用。
//
// 引用不复制正文：记忆只有一份，指针可以有很多条。
// 但指针必须指向一份还活着的切片——悬空指针在 gc 之后就成了死链，
// 而那时已经没人能解释它原本指向什么。
func (s *Server) putRef(target, refAddr, session string, c Caller) (TermResult, error) {
	srcProj, srcBranch, srcPath, err := s.resolveAddr(refAddr, c)
	if err != nil {
		return TermResult{}, fmt.Errorf("引用目标 %q 无效: %w", refAddr, err)
	}
	if !s.allowed(c, srcProj, srcBranch, configBitRead) {
		return TermResult{}, fmt.Errorf("无权读取引用目标 %s@%s", srcProj, srcBranch)
	}
	repo, err := s.projects.Get(srcProj)
	if err != nil {
		return TermResult{}, err
	}
	data, err := repo.Read(srcBranch, srcPath)
	if err != nil || len(data) == 0 {
		return TermResult{}, fmt.Errorf("引用目标 %s@%s/%s 不存在", srcProj, srcBranch, srcPath)
	}

	body := fmt.Sprintf("ref: %s@%s/%s\n", srcProj, srcBranch, srcPath)
	spec := s.parsePutSpec(target, c)
	path, proj, branch, err := s.writeNoteGit("", body, session, c.Role, c, spec.Explicit, spec.TreePath)
	if err != nil {
		return TermResult{}, err
	}
	rel := strings.TrimPrefix(path, proj+"/")

	dstRepo, err := s.projects.Get(proj)
	if err != nil {
		return TermResult{}, err
	}
	commitSHA, _ := dstRepo.Resolve(branch)
	virtPath := c.Mount.ToVirtual(proj, branch, rel)

	text := "ok " + virtPath
	if commitSHA != "" {
		text += " (base " + shortSHA(commitSHA) + ")"
	}
	return TermResult{
		Text: text,
		Structured: map[string]any{
			"ok":      true,
			"queued":  true,
			"pending": dstRepo.Pending(branch),
			"path":    virtPath,
			"ref":     refAddr,
			"project": proj,
			"branch":  branch,
			"base":    commitSHA,
			"clock":   s.Clock(),
		},
	}, nil
}

// notifyTreeChange 在分支头推进后计算差量并广播。
//
// 只要这次提交有父提交，就用 SubtreeDiff 算出实际触及的路径集合；
// 客户端能拿到精准的前缀，没动过的子树绝不收事件。
func (s *Server) notifyTreeChange(project, branch, relPath string) {
	repo, err := s.projects.Get(project)
	if err != nil {
		return
	}
	commitSHA, _ := repo.Resolve(branch)
	if commitSHA == "" {
		return
	}
	treeSHA, _ := repo.TreeAt(branch, "")

	ev := TreeEvent{
		Project: project,
		Branch:  branch,
		Commit:  commitSHA,
		Tree:    treeSHA,
	}

	// 取上一个提交（父提交）作为差分基线。
	if _, parents, err := repo.CommitInfoAt(commitSHA); err == nil && len(parents) > 0 {
		ev.Previous = parents[0]
		{
			if diff, err := repo.SubtreeDiff(ev.Previous, commitSHA, ""); err == nil {
				paths := make([]string, 0, len(diff))
				for _, ch := range diff {
					paths = append(paths, ch.Path)
					if len(paths) >= maxEventPaths {
						ev.Truncated = true
						break
					}
				}
				ev.Paths = paths
			}
		}
	}

	if relPath != "" {
		// 单条写入明确知道自己落在哪，直接补齐，防止 diff 计算失败漏通知。
		found := false
		for _, p := range ev.Paths {
			if p == relPath {
				found = true
				break
			}
		}
		if !found {
			ev.Paths = append(ev.Paths, relPath)
		}
	}

	s.broadcastTreeEvent(ev)
}

// execRemind 把当前身份锚定到一棵树上：那棵树动了就通知我。
//
// remind 不是另一个动词的缩写，也不是消息订阅——它是「注意力锚点」。
// 锚定关系是派生态：丢了最多重读一次，所以不写进不可变历史。
func (s *Server) execRemind(rest string, c Caller) (TermResult, error) {
	rest = strings.TrimSpace(rest)

	if rest == "" {
		watches := s.treeWatch.List(c.Identity)
		if len(watches) == 0 {
			return TermResult{
				Text:       "当前没有锚定任何树。用 remind <project>[@<branch>[/<path>]] 锚定一棵。",
				Structured: map[string]any{"watches": []TreeWatch{}},
			}, nil
		}
		var sb strings.Builder
		sb.WriteString("当前锚定：\n")
		for _, w := range watches {
			p := w.Path
			if p == "" {
				p = "(整棵树)"
			}
			fmt.Fprintf(&sb, "  %s@%s %s\n", w.Project, w.Branch, p)
		}
		return TermResult{
			Text:       strings.TrimRight(sb.String(), "\n"),
			Structured: map[string]any{"watches": watches},
		}, nil
	}

	if strings.HasPrefix(rest, "-") {
		s.treeWatch.Clear(c.Identity)
		return TermResult{
			Text:       "已取消全部锚点",
			Structured: map[string]any{"ok": true, "cleared": true},
		}, nil
	}

	proj, branch, treePath, err := s.parseBranchAddr(rest)
	if err != nil {
		return TermResult{}, err
	}
	if !s.projectVisibleFor(c, proj) {
		return TermResult{}, fmt.Errorf("项目 %q 不在这张凭证的可见范围内", proj)
	}
	if !s.allowed(c, proj, branch, configBitRead) {
		return TermResult{}, fmt.Errorf("%s", s.denyReason(c, proj, branch, configBitRead))
	}

	s.treeWatch.Add(c.Identity, TreeWatch{Project: proj, Branch: branch, Path: treePath})
	where := proj + "@" + branch
	if treePath != "" {
		where += "/" + treePath
	}
	return TermResult{
		Text: "已锚定 " + where + "：这棵树变动时会收到脉冲",
		Structured: map[string]any{
			"ok":      true,
			"project": proj,
			"branch":  branch,
			"path":    treePath,
		},
	}, nil
}

// execTrees 列出某个分支下的树结构（只到目录一级）。
//
// 给调用方一个「我锚在哪」的地图：树路径不是猜出来的。
func (s *Server) execTrees(rest string, c Caller) (TermResult, error) {
	addr := strings.TrimSpace(rest)
	if addr == "" {
		return TermResult{}, errors.New("trees 需要 <project>[@<branch>]")
	}
	proj, branch, prefix, err := s.parseBranchAddr(addr)
	if err != nil {
		return TermResult{}, err
	}
	if !s.allowed(c, proj, branch, configBitRead) {
		return TermResult{}, fmt.Errorf("%s", s.denyReason(c, proj, branch, configBitRead))
	}
	repo, err := s.projects.Get(proj)
	if err != nil {
		return TermResult{}, err
	}
	entries, err := repo.List(branch)
	if err != nil {
		return TermResult{}, err
	}

	seen := map[string]bool{}
	var dirs []string
	prefix = strings.Trim(prefix, "/")
	for _, e := range entries {
		p := e.Path
		if prefix != "" {
			if !strings.HasPrefix(p, prefix+"/") {
				continue
			}
			p = strings.TrimPrefix(p, prefix+"/")
		}
		head, _, hasSlash := strings.Cut(p, "/")
		if !hasSlash {
			continue
		}
		full := head
		if prefix != "" {
			full = prefix + "/" + head
		}
		if !seen[full] {
			seen[full] = true
			dirs = append(dirs, full)
		}
	}
	if len(dirs) == 0 {
		return TermResult{Text: "（没有子目录）"}, nil
	}
	var sb strings.Builder
	for _, d := range dirs {
		fmt.Fprintf(&sb, "%s\n", d)
	}
	return TermResult{
		Text:       strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{"project": proj, "branch": branch, "trees": dirs},
	}, nil
}

// parseTreeAddress 解析显式或相对树地址。
//
// 支持：project@branch/path、project/path、mount 虚拟路径以及相对树路径。
// 相对路径走当前身份的默认可写目标，而不是猜全局第一个仓库。
func (s *Server) parseTreeAddress(addr string, c Caller) (project, branch, treePath string, err error) {
	addr = strings.Trim(strings.TrimSpace(addr), "/")
	if strings.Contains(addr, "@") {
		return s.parseBranchAddr(addr)
	}
	if !c.Mount.Empty() {
		if p, b, path, e := c.Mount.ToReal(addr); e == nil {
			return p, b, path, nil
		}
	}
	for _, p := range s.declaredProjects() {
		if addr == p {
			br := s.persona
			if br == "" {
				br = "zero"
			}
			return p, br, "", nil
		}
		if strings.HasPrefix(addr, p+"/") {
			br := s.persona
			if br == "" {
				br = "zero"
			}
			return p, br, strings.TrimPrefix(addr, p+"/"), nil
		}
	}
	p, br, _, e := s.writeTarget(c, s.persona)
	if e != nil {
		return "", "", "", e
	}
	return p, br, addr, nil
}

// splitScope 从查询里剥出 --scope <tree> 参数。
//
// 默认不限制范围：调用方知道的树路径未必完整，强制写全会让
// 「随口问一句」变成查参数。需要收窄时显式给。
func splitScope(rest string) (query, scope string) {
	query = strings.TrimSpace(rest)
	for {
		i := strings.Index(query, "--scope")
		if i < 0 {
			return query, scope
		}
		before := strings.TrimSpace(query[:i])
		after := strings.TrimSpace(query[i+len("--scope"):])
		fields := strings.Fields(after)
		if len(fields) == 0 {
			return before, scope
		}
		scope = strings.Trim(fields[0], "/")
		query = strings.TrimSpace(before + " " + strings.Join(fields[1:], " "))
	}
}

// addrUnderScope 判断一个索引地址是否落在给定树范围里。
//
// 地址形如 project@branch/path；范围可以是 project、project@branch
// 或 project@branch/subtree。
func addrUnderScope(addr, scope string) bool {
	proj, branch, path, ok := memory.SplitID(addr)
	if !ok {
		return false
	}
	full := proj + "@" + branch
	if scope == proj || scope == full {
		return true
	}
	if strings.HasPrefix(scope, full+"/") {
		sub := strings.TrimPrefix(scope, full+"/")
		return path == sub || strings.HasPrefix(path, sub+"/")
	}
	return false
}

// execSince 列出某棵子树自给定 commit 之后的增量变化。
//
// 这是断线续传的入口：调用方记住上次读到的 commit，重连后换回差量。
// 子树 SHA 相同则直接返回空——没动过的子树代价是零，这条短路是
// 「按树订阅」真正的性能来源。
func (s *Server) execSince(rest string, c Caller) (TermResult, error) {
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return TermResult{}, errors.New("since 语法: since <tree> <commit|empty>")
	}
	proj, branch, treePath, err := s.parseTreeAddress(fields[0], c)
	if err != nil {
		return TermResult{}, err
	}
	oldCommit := strings.TrimSpace(fields[1])
	if oldCommit == "empty" || oldCommit == "-" {
		oldCommit = ""
	} else if len(oldCommit) != 40 {
		return TermResult{}, fmt.Errorf("since 需要完整 commit SHA（40 位），或首次同步时用 empty；得到 %q", oldCommit)
	}
	if !s.allowed(c, proj, branch, configBitRead) {
		return TermResult{}, fmt.Errorf("%s", s.denyReason(c, proj, branch, configBitRead))
	}
	repo, err := s.projects.Get(proj)
	if err != nil {
		return TermResult{}, err
	}
	head, err := repo.Resolve(branch)
	if err != nil {
		return TermResult{}, err
	}
	if head == "" {
		return TermResult{Text: "（该分支还没有提交）"}, nil
	}

	var changes []gitstore.Change
	if oldCommit == "" {
		if treePath == "" {
			changes, err = repo.Diff("", branch)
		} else {
			changes, err = repo.SubtreeDiff("", head, treePath)
		}
	} else {
		changes, err = repo.SubtreeDiff(oldCommit, head, treePath)
	}
	if err != nil {
		// 游标不可达（分支被 reset/archive 过）时退回提示全量重读，
		// 而不是猜一个近似的起点——那会让漏读变成静默事实。
		return TermResult{}, fmt.Errorf("无法从 %s 计算增量（该提交可能已不可达）：%w\n请用 log 重新确定起点，或整棵重读", oldCommit, err)
	}

	cur, _ := repo.TreeAt(head, treePath)
	if len(changes) == 0 {
		return TermResult{
			Text: "（这棵树自该提交以来没有变化）",
			Structured: map[string]any{
				"project": proj, "branch": branch, "path": treePath,
				"from": oldCommit, "to": head, "tree": cur, "changes": []any{},
			},
		}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s@%s", proj, branch)
	if treePath != "" {
		sb.WriteString("/" + treePath)
	}
	fromLabel := "empty"
	if oldCommit != "" {
		fromLabel = shortSHA(oldCommit)
	}
	fmt.Fprintf(&sb, " 自 %s 起的变化（共 %d 条）：\n", fromLabel, len(changes))
	items := make([]any, 0, len(changes))
	for _, ch := range changes {
		fmt.Fprintf(&sb, "  %-9s %s\n", ch.Kind, ch.Path)
		items = append(items, map[string]any{"kind": ch.Kind, "path": ch.Path})
	}
	return TermResult{
		Text: strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{
			"project": proj, "branch": branch, "path": treePath,
			"from": oldCommit, "to": head, "tree": cur, "changes": items,
		},
	}, nil
}
