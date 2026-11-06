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
		return TermResult{}, fmt.Errorf("credential lacks w capability")
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
			return TermResult{}, errors.New("put --ref requires an address")
		}
	} else {
		if !hasBody {
			return TermResult{}, errors.New("put syntax: put [<tree>] -- $TEXT or put <tree> --ref <addr>")
		}
		if body == "" {
			return TermResult{}, errors.New("content cannot be empty")
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

	// 显式分支：proj@branch[:path] 或旧 proj@branch[/path]
	if at := strings.Index(target, "@"); at >= 0 {
		proj, br, p, err := s.parseBranchAddr(target)
		if err == nil && proj != "" {
			return putSpec{Explicit: proj + "@" + br, TreePath: strings.Trim(p, "/")}
		}
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

	// source 是审计标签：这条记忆从哪个设备/实例写进来。
	// 它只是数据，不参与任何权限判定。
	source := c.Identity
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
		return TermResult{}, fmt.Errorf("reference target %q is invalid: %w", refAddr, err)
	}
	if !s.allowed(c, srcProj, srcBranch, configBitRead) {
		return TermResult{}, fmt.Errorf("not authorized to read reference target %s@%s", srcProj, srcBranch)
	}
	repo, err := s.projects.Get(srcProj)
	if err != nil {
		return TermResult{}, err
	}
	data, err := repo.Read(srcBranch, srcPath)
	if err != nil || len(data) == 0 {
		return TermResult{}, fmt.Errorf("reference target %s@%s/%s does not exist", srcProj, srcBranch, srcPath)
	}

	body := fmt.Sprintf("ref: %s@%s/%s\n", srcProj, srcBranch, srcPath)
	spec := s.parsePutSpec(target, c)
	path, proj, branch, err := s.writeNoteGit("", body, session, c.Identity, c, spec.Explicit, spec.TreePath)
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

// execRemind 登记「我关心这棵树」，并记下此刻的分支头作为基线。
//
// 它不订阅任何东西、不推送任何东西——本服务只有 MCP 一个通道。
// 变更感知靠调用方主动查：status 报「关注的树动了没有」，
// since 取真正的差量。
//
// 为什么保留这个动词：它把「我关心什么」这件事记在服务端，
// 于是调用方不必自己在客户端维护一份列表，也能让 status 回答
// 「有没有该看的新东西」。这是纯粹的状态登记，零副作用。
func (s *Server) execRemind(rest string, c Caller) (TermResult, error) {
	rest = strings.TrimSpace(rest)

	if rest == "" {
		watches := s.watchReport(c)
		if len(watches) == 0 {
			return TermResult{
				Text:       "no tree is watched. Use remind <project>[@<branch>[/<path>]] to record one, or `status` to see all watched trees.",
				Structured: map[string]any{"watches": []watchStatus{}},
			}, nil
		}
		lines := append([]string{"currently watched:"}, formatWatchReport(watches)...)
		return TermResult{
			Text:       strings.Join(lines, "\n"),
			Structured: map[string]any{"watches": watches},
		}, nil
	}

	if strings.HasPrefix(rest, "-") {
		s.treeWatch.Clear(c.Identity)
		return TermResult{
			Text:       "all watches cleared",
			Structured: map[string]any{"ok": true, "cleared": true},
		}, nil
	}

	proj, branch, treePath, err := s.parseBranchAddr(rest)
	if err != nil {
		return TermResult{}, err
	}
	if !s.projectVisibleFor(c, proj) {
		return TermResult{}, fmt.Errorf("project %q is outside this credential's visible scope", proj)
	}
	if !s.allowed(c, proj, branch, configBitRead) {
		return TermResult{}, fmt.Errorf("%s", s.denyReason(c, proj, branch, configBitRead))
	}

	// 记下当前头作为基线：下次 status 就能回答「它动了没有」。
	// 分支还不存在时留空——关注一个尚未创建的分支是合理的，
	// 只是暂时答不了「变没变」。
	since := ""
	repo, rerr := s.projects.Get(proj)
	if rerr == nil {
		if head, herr := repo.Resolve(branch); herr == nil {
			since = head
		}
	}

	s.treeWatch.Add(c.Identity, TreeWatch{Project: proj, Branch: branch, Path: treePath, Since: since})
	where := proj + "@" + branch
	if treePath != "" {
		where += "/" + treePath
	}
	msg := "watching " + where + " from " + shortSHA(since)
	if since == "" {
		msg = "watching " + where + " (branch is empty; baseline will be set on next remind)"
	}
	return TermResult{
		Text: msg + " — check it with `status`, fetch changes with `since`",
		Structured: map[string]any{
			"ok":      true,
			"project": proj,
			"branch":  branch,
			"path":    treePath,
			"since":   since,
		},
	}, nil
}

// execTrees 列出某个分支下的树结构（只到目录一级）。
//
// 给调用方一个「我锚在哪」的地图：树路径不是猜出来的。
func (s *Server) execTrees(rest string, c Caller) (TermResult, error) {
	addr := strings.TrimSpace(rest)
	if addr == "" {
		return TermResult{}, errors.New("trees requires <project>[@<branch>]")
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
		return TermResult{Text: "(no subdirectories)"}, nil
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
			br := s.defaultBranch
			return p, br, "", nil
		}
		if strings.HasPrefix(addr, p+"/") {
			br := s.defaultBranch
			return p, br, strings.TrimPrefix(addr, p+"/"), nil
		}
	}
	p, br, _, e := s.writeTarget(c, s.defaultBranch)
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
			break
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
	return query, scope
}

// unknownFlag 找出 DSL 里被误当搜索词的选项。
//
// 为什么必须报错而不是忽略：`grep -c . zero-agi` 的真实后果是
// 把 "-c" 编进正则、把 "zero-agi" 当搜索词，最后返回 "no matches"——
// 调用方会得出「库里没有」这个错误结论，而事实是参数写错了。
// 静默比报错危险得多：报错会停下来，静默会接着往下推。
func unknownFlag(rest string) (string, bool) {
	for _, f := range strings.Fields(rest) {
		if !strings.HasPrefix(f, "-") {
			continue
		}
		// 孤立的 "-" 不算选项（可能是正则的一部分）。
		if f == "-" {
			continue
		}
		if f == "--scope" {
			continue
		}
		return f, true
	}
	return "", false
}

// addrUnderScope 判断一个索引地址是否落在给定树范围里。
//
// 地址形如 project@branch/path；范围可以是 project、project@branch
// 或 project@branch/subtree。
// addrUnderScope 判断地址是否在某个 scope 之下。
//
// scope 的两种写法：
//
//	<project>                整个仓库
//	<project>@<ref>          某个分支（ref 可含斜杠，如 main/record）
//	<project>@<ref>:<path>   某个分支下的子路径
//
// 注意 ref 与 path 之间是冒号，ref 内部的斜杠属于分支层级——
// 这是地址语法的根本分界，不能再用 "/" 去区分两者。
func addrUnderScope(addr, scope string) bool {
	proj, branch, path, ok := memory.SplitID(addr)
	if !ok {
		return false
	}
	if scope == proj {
		return true
	}
	// scope 带 @ref 时，可能还带 :path
	sp, sb, spath, okScope := memory.SplitID(scope)
	if !okScope {
		return false
	}
	if sp != proj || sb != branch {
		return false
	}
	if spath == "" {
		return true
	}
	return path == spath || strings.HasPrefix(path, spath+"/")
}

// execSince 列出某棵子树自给定 commit 之后的增量变化。
//
// 这是断线续传的入口：调用方记住上次读到的 commit，重连后换回差量。
// 子树 SHA 相同则直接返回空——没动过的子树代价是零，这条短路是
// 「按树订阅」真正的性能来源。
func (s *Server) execSince(rest string, c Caller) (TermResult, error) {
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return TermResult{}, errors.New("since syntax: since <tree> <commit|empty>")
	}
	proj, branch, treePath, err := s.parseTreeAddress(fields[0], c)
	if err != nil {
		return TermResult{}, err
	}
	oldCommit := strings.TrimSpace(fields[1])
	if oldCommit == "empty" || oldCommit == "-" {
		oldCommit = ""
	} else if len(oldCommit) != 40 {
		return TermResult{}, fmt.Errorf("since requires a full commit SHA (40 characters), or empty for the first sync; got %q", oldCommit)
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
		return TermResult{Text: "(this branch has no commits yet)"}, nil
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
		return TermResult{}, fmt.Errorf("cannot compute changes from %s (the commit may be unreachable): %w\nUse log to choose a new starting point, or reread the entire tree", oldCommit, err)
	}

	cur, _ := repo.TreeAt(head, treePath)
	if len(changes) == 0 {
		return TermResult{
			Text: "(this tree has not changed since that commit)",
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
	fmt.Fprintf(&sb, " changes since %s (%d total):\n", fromLabel, len(changes))
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
