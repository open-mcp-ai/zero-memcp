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

	// 不带 @ 的分支写法：<project>/<branch>[/path] 或 <branch>[/path]。
	//
	// 必须在「已声明项目前缀」之前判：`BlackEggAI/zero/main.shared/x.md`
	// 既像项目路径也像项目/分支/路径，后者才是调用方的意思。
	// 判据是那一段真的是分支名（取自 refs），不是猜。
	if proj, br, rest, ok := s.splitBranchPath(target); ok {
		return putSpec{Explicit: proj + "@" + br, TreePath: strings.Trim(rest, "/")}
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
	// 屏障：等这一笔真的落进脏层，再决定回什么。
	//
	// 这里曾经直接回 ok——Write 只承诺「已投递」。实测出现过
	// put 回 ok、路径也对，但文件在磁盘和 git 里都不存在，
	// 随后 commit 回 "nothing pending" 的情况。丢写不报错是最贵的故障。
	if err := repo.Barrier(); err != nil {
		return TermResult{}, fmt.Errorf("write did not reach disk: %w", err)
	}
	// 回读校验：脏层能读到，才算真的在。
	if _, rerr := repo.Read(branch, rel); rerr != nil {
		return TermResult{}, fmt.Errorf(
			"write reported success but %s@%s:%s cannot be read back (%v); treating it as failed",
			proj, branch, rel, rerr)
	}

	commitSHA, _ := repo.Resolve(branch)
	virtPath := s.virtualPath(c, proj, branch, rel)

	// 登记进命令流：这次写入从此有一个位点，对端按位点增量取走。
	//
	// 放在回读校验之后：只有确认真的落进脏层的写入才配拥有位点，
	// 否则日志里会出现一条指向不存在内容的命令。
	if e, ok, serr := repo.ReadPathSHA(branch, rel); serr == nil && ok {
		s.noteWriteOp(proj, branch, rel, string(e.Mode), e.SHA)
	} else if serr != nil {
		s.logf("oplog: cannot resolve written blob for %s@%s:%s: %v", proj, branch, rel, serr)
	}

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

	// 清空全部关注：`remind --clear`（也接受单独的 -），但**不能**
	// 把任意一个 flag 都当成清空。
	//
	// 踩过的坑（2026-11-22）：语法层把 flag 还原成 `--reanchor` 追加在
	// 地址后面，当地址为空时就只剩 `--reanchor`——旧判据 HasPrefix("-")
	// 把它当成「清空全部」，于是一次重新锤定把一个身份的关注表抹平。
	// 破坏性操作的判据必须是精确匹配，不能是前缀。
	if rest == "-" || rest == "--clear" || rest == "-c" {
		if err := s.treeWatch.Clear(c.Identity); err != nil {
			return TermResult{}, err
		}
		return TermResult{
			Text:       "all watches cleared",
			Structured: map[string]any{"ok": true, "cleared": true},
		}, nil
	}

	// 只接受一个地址（可带 --reanchor）。多出来的参数一定是把
	// `since <tree> <commit>` 的写法用在了这里——整段塞进分支名会产生一条
	// 永远不生效的订阅（3060 hub 上真的出现过 branch = "main.shared e8103f18..."）。
	// 与其静默吞掉，不如当场告诉他该用哪个动词。
	reanchor := false
	addr := rest
	if fields := strings.Fields(rest); len(fields) > 1 {
		kept := make([]string, 0, len(fields))
		for _, f := range fields {
			switch f {
			case "--reanchor":
				reanchor = true
			case "-r":
				reanchor = true
			default:
				kept = append(kept, f)
			}
		}
		if len(kept) > 1 {
			return TermResult{}, fmt.Errorf("remind takes a single tree address, got %q; "+
				"to fetch changes from a known commit use `since <tree> <commit>`", rest)
		}
		addr = strings.Join(kept, " ")
	}

	proj, branch, treePath, err := s.parseBranchAddr(addr)
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

	if err := s.treeWatch.Add(c.Identity, TreeWatch{Project: proj, Branch: branch, Path: treePath, Since: since}, reanchor); err != nil {
		return TermResult{}, err
	}
	// 报出**实际生效的**基线，而不是刚刚算出的当前头。
	//
	// 幂等登记时服务端保留旧基线，若此处照旧报 since，调用方会以为
	// 基线已经推到新头，而实际没有——两边理解不一致比不报还坏。
	effective := since
	keptBaseline := false
	for _, w := range s.treeWatch.List(c.Identity) {
		if w.Project == proj && w.Branch == branch && w.Path == treePath {
			effective = w.Since
			keptBaseline = !reanchor && effective != "" && effective != since
			break
		}
	}
	where := proj + "@" + branch
	if treePath != "" {
		where += "/" + treePath
	}
	msg := "watching " + where + " from " + shortSHA(effective)
	if reanchor {
		msg = "re-anchored " + where + " at " + shortSHA(effective)
	}
	if keptBaseline {
		msg += " (baseline kept; use --reanchor to reset it)"
	}
	if effective == "" {
		msg = "watching " + where + " (branch is empty; baseline will be set on next remind)"
	}
	return TermResult{
		Text: msg + " — check it with `status`, fetch changes with `since`",
		Structured: map[string]any{
			"ok":            true,
			"project":       proj,
			"branch":        branch,
			"path":          treePath,
			"since":         effective,
			"baseline_kept": keptBaseline,
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
		// 没写 @branch 且默认分支不可见时，别用「不在可见范围内」把人挡死：
		// 列出这张凭证真正能看的几条分支，比一句拒绝有用得多。
		// （实测：dev 执行 trees BlackEggAI/zero 报 outside visible scope，
		//   而它明明能读 main / main.record / main.dev / main.shared。）
		if !strings.Contains(addr, "@") {
			if vis := s.visibleBranches(c, proj); len(vis) > 0 {
				var sb strings.Builder
				sb.WriteString("no branch specified and the default branch is not visible; visible branches of " + proj + ":\n")
				for _, b := range vis {
					fmt.Fprintf(&sb, "  %s@%s\n", proj, b)
				}
				sb.WriteString("retry as: trees " + proj + "@<branch>")
				return TermResult{Text: sb.String()}, nil
			}
		}
		return TermResult{}, fmt.Errorf("%s", s.denyReason(c, proj, branch, configBitRead))
	}
	// 目录从内存树里取，不再 repo.List 一遍。
	//
	// repo.List 会展开整条分支（main.chat = 5233 个路径，实测 751ms），
	// 而这里只要目录名。内存树本来就是索引在用的那份，是同步过的：
	// put 走 addBlockLocked、write/delete 走 refreshPath、push 走
	// refreshBranch，都会立即更新它。
	//
	// 代价是「列出目录」依赖索引已加载；这在服务起来后恒成立
	// （Reload 在 Listen 之前跑完），和 ls 用的是同一份数据。
	paths := s.indexedPaths(proj, branch)
	if paths == nil {
		// 索引里没有这条分支（刚声明还没写过）：退回真扫一次。
		repo, err := s.projects.Get(proj)
		if err != nil {
			return TermResult{}, err
		}
		entries, err := repo.List(branch)
		if err != nil {
			return TermResult{}, err
		}
		paths = make([]string, 0, len(entries))
		for _, e := range entries {
			paths = append(paths, e.Path)
		}
	}

	seen := map[string]bool{}
	var dirs []string
	prefix = strings.Trim(prefix, "/")
	for _, p := range paths {
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
	q, scopes := splitScopes(rest)
	if len(scopes) == 0 {
		return q, ""
	}
	return q, scopes[0]
}

// splitScopes 解析 `--scope A` 以及 `--scope A||B||C`（分层召回）。
//
// 为什么要支持 `||`：扩展的分层召回一直发的是
// `recall $Q --scope main||main.record||main.dev`，而服务端只认单个
// scope——`||` 在 DSL 里是语句分隔符，于是整条命令被拆成
// 「recall ... --scope main」+「main.record」+「main.dev」，
// 后两段是非法指令。症状是分层召回 100% 失败、且失败被静默吞掉：
// 调用方看到的是「没有命中」，于是以为记忆里真的没有。
//
// 这里在**语句切分之前**就把 `||` 认成 scope 分隔符，所以
// `a||b||c` 不会被当成三条语句。
// stripJSONFlag 摘掉 --json 并报告是否出现过。
//
// 为什么 --json 不走 splitScopes：它是「怎么返回」而不是「查什么」，
// 混进 query 会让它变成搜索词（--json 被当成一个 token 参与匹配），
// 症状是「加了 --json 之后结果全变了」而没有任何报错。
func stripJSONFlag(rest string) (string, bool) {
	want := false
	out := make([]string, 0, 4)
	for _, f := range strings.Fields(rest) {
		if f == "--json" {
			want = true
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " "), want
}

func splitScopes(rest string) (query string, scopes []string) {
	query = strings.TrimSpace(rest)
	for {
		i := strings.Index(query, "--scope")
		if i < 0 {
			break
		}
		before := strings.TrimSpace(query[:i])
		after := strings.TrimSpace(query[i+len("--scope"):])
		if after == "" {
			query = before
			break
		}
		// 取到第一个空白为止：scope 里不会有空格，但可能有 ||。
		field := after
		rest := ""
		if j := strings.IndexAny(after, " \t\n"); j >= 0 {
			field, rest = after[:j], strings.TrimSpace(after[j:])
		}
		// 逗号是折叠后的形状（protectLayeredScope 干的），竖线是原始写法。
		for _, part := range strings.FieldsFunc(field, func(r rune) bool { return r == ',' || r == '|' }) {
			part = strings.Trim(strings.TrimSpace(part), "/")
			if part != "" {
				scopes = append(scopes, part)
			}
		}
		query = strings.TrimSpace(before + " " + rest)
	}
	return query, scopes
}

// matchAnyScope 判断地址是否落在任一 scope 内。
// 空 scopes 表示不限制（与「没有 --scope」等价）。
func matchAnyScope(addr string, scopes []string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, sc := range scopes {
		if addrUnderScope(addr, sc) {
			return true
		}
	}
	return false
}

// unknownFlag 找出 DSL 里被误当搜索词的选项。
//
// 为什么必须报错而不是忽略：`grep -c . zero-agi` 的真实后果是
// 把 "-c" 编进正则、把 "zero-agi" 当搜索词，最后返回 "no matches"——
// 调用方会得出「库里没有」这个错误结论，而事实是参数写错了。
// 静默比报错危险得多：报错会停下来，静默会接着往下推。
// hasFlag 报告 rest 里是否包含某个选项。
func hasFlag(rest string, flags ...string) bool {
	for _, f := range strings.Fields(rest) {
		for _, want := range flags {
			if f == want {
				return true
			}
		}
	}
	return false
}

// stripFlags 从 rest 里剥掉给定的选项（完整 token 匹配）。
func stripFlags(rest string, flags ...string) string {
	drop := map[string]bool{}
	for _, f := range flags {
		drop[f] = true
	}
	var keep []string
	for _, f := range strings.Fields(rest) {
		if !drop[f] {
			keep = append(keep, f)
		}
	}
	return strings.Join(keep, " ")
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
		if f == "--scope" || f == "--json" {
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

// execRef 实现引用写入：`ref [<tree>] @<session> <addr>`。
//
// 为什么它是独立动词而不是 `put --ref`：termcp 的 PayloadVar 要求 payload
// 必须存在，而引用写入没有正文。一个 Op 声明不了「有正文」和「无正文」
// 两种形态，硬塞在一起就只能放弃 PayloadVar——那等于放弃协议层对正文
// 边界的强制。拆开之后 put 走 PayloadVar，ref 走位置参数，各归各的。
func (s *Server) execRef(rest string, c Caller) (TermResult, error) {
	if !c.canWrite() {
		return TermResult{}, fmt.Errorf("credential lacks w capability")
	}
	var session string
	var positional []string
	for _, tok := range strings.Fields(rest) {
		if strings.HasPrefix(tok, "@") && !strings.Contains(tok, "/") {
			session = strings.TrimPrefix(tok, "@")
			continue
		}
		positional = append(positional, tok)
	}
	// 最后一个非会话词是引用源，其余（若有）是落点树。
	//
	// 顺序固定，不能用「含不含斜杠」去猜：地址与树路径都可能含斜杠，
	// 猜错就把引用写进了错误的树。`ref <addr>`（缺省树）也由这条覆盖。
	if len(positional) == 0 {
		return TermResult{}, errors.New("usage: ref <tree> <addr>")
	}
	refAddr := positional[len(positional)-1]
	tree := strings.Join(positional[:len(positional)-1], " ")
	return s.putRef(tree, refAddr, session, c)
}
