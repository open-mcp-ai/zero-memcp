package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/manifest"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// writeNoteGit 把一条记忆写进可写仓库的当前分支。
//
// 文件名完全是时间戳：YYYY-MM-DD/150405.000-endpoint.md。
// 不带任何 checksum 尾巴：
//   - Git 底层本身就是内容寻址（Merkle 树），每个 blob 的 SHA-1 就是它的真名；
//   - 幂等去重直接通过比较已存在条目的 Blob SHA 实现，既精准又不污染文件名；
//   - 时间戳本身就是切片的自然时间坐标，人类与机器一眼可读。
func (s *Server) writeNoteGit(kind, text, session, source string, c Caller, explicit, treePrefix string) (addr, project, branch string, err error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	// 空 kind 表示「由落点决定」：put 不再要求调用方先给自己分类。
	implicitKind := kind == ""

	// 这两类永远拒绝，无论 SKILL.md 怎么声明。
	//
	// 它们是硬约束，不是策略：允许写入「指令」等于允许记忆库自我修改行为，
	// 而记忆库的写入方是模型自己。声明能改的东西里不能包含这条。
	switch kind {
	case "instruction", "rule":
		return "", "", "", errors.New("安全拒绝：严禁写入 instruction 或 rule（推断出的指令不进记忆库）")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", "", errors.New("文本不能为空")
	}

	persona := s.persona
	if persona == "" {
		persona = "zero"
	}
	// 写入分支：显式挂载点优先，否则用默认人格分支。
	writeBranch := persona
	if !c.Mount.Empty() {
		writeBranch = c.Mount.DefaultBranch()
	}

	// 显式目标里写了 @<branch> 时，分支也跟着它走。
	//
	// 这是审计 Agent 的关键路径：它读完脏流，要把提纯后的内容写进
	// main / dev / chat / private 中的某一条，而它自己的人格分支
	// 跟这些都不是一回事。没有 @branch，它只能往自己分支上写，
	// 然后再多一步 mv——两跳之间被杀就留下半吊子状态。
	if explicit != "" {
		if at := strings.Index(explicit, "@"); at >= 0 {
			proj := explicit[:at]
			rest := explicit[at+1:]
			br, _, _ := strings.Cut(rest, "/")
			if br != "" {
				if !c.Mount.Visible(proj, br) {
					return "", "", "", fmt.Errorf("项目 %s@%s 不在这张凭证的可见范围内", proj, br)
				}
				if !s.allowed(c, proj, br, configBitWrite) {
					return "", "", "", errors.New(s.denyReason(c, proj, br, configBitWrite))
				}
				writeBranch = br
				explicit = proj
			}
		}
	}

	// 写入目标必须同时满足三件事：挂载表可见、有写权限、配置里声明过。
	// 取「全局第一个项目」是错的——那张凭证可能根本看不见它。
	targetProject, resolvedBranch, pol, err := s.resolveWriteTarget(c, persona, explicit)
	if err != nil {
		return "", "", "", err
	}
	// 挂载表解析出的分支优先：chat 角色的 /daily 指向 daily@chat，
	// 而不是它的人格分支 zero。忽略它会让「用 chat 身份写」
	// 全都落进 zero 分支，权限看着对、内容却混在一起。
	if resolvedBranch != "" {
		writeBranch = resolvedBranch
	}
	if implicitKind {
		// 落点决定类型：项目声明的第一个 kind 就是它的默认语义。
		if len(pol.Kinds) > 0 {
			kind = pol.Kinds[0]
		} else {
			kind = "fact"
		}
	} else if !pol.AllowsKind(kind) {
		return "", "", "", fmt.Errorf("kind %q 不在本项目的写入策略里（允许 %s）",
			kind, strings.Join(pol.Kinds, " / "))
	}

	repo, err := s.projects.Ensure(targetProject)
	if err != nil {
		return "", "", "", fmt.Errorf("打开写入目标 %s: %w", targetProject, err)
	}

	now := time.Now()
	endpoint := s.cfg.Endpoint
	if endpoint == "" {
		endpoint = "local"
	}

	if source == "" {
		source = "mcp"
	}
	sessionLine := ""
	if session != "" {
		sessionLine = fmt.Sprintf("session: %s\n", session)
	}
	// 类型由落点决定：显式给了 kind 才写进 frontmatter。
	// 写一条记忆还要先声明「我是哪种」，是多出来的一步自我分类。
	kindLine := ""
	if !implicitKind {
		kindLine = fmt.Sprintf("kind: %s\n", kind)
	}
	content := fmt.Sprintf("---\n%ssource: %s\nendpoint: %s\ncreated: %s\n%s---\n\n%s\n",
		kindLine, source, endpoint, now.Format(time.RFC3339), sessionLine, text)

	// 幂等判重：按「语义内容」而不是整份文件。
	//
	// 早先的做法是把整份带 frontmatter 的 content 算 Git blob hash，
	// 但 created 时间戳每次不同——同一段文字第二次写，哈希必然不等，
	// 判重分支从未执行过。症状是「插件以为服务端会去重，服务端其实不会」，
	// 水位文件一丢就整段重复落盘。
	//
	// 现在的指纹 = kind + session + 正文。这是「这条记忆是什么」，
	// 而不是「它被写在哪份文件里」。
	// SKILL.md 可以为整个项目声明默认写入树。
	if treePrefix == "" && pol.Tree != "" {
		treePrefix = pol.Tree
	}
	// 树前缀进入判重键：同一段正文写进两棵不同的树，是两次独立写入。
	// 判重键里的类型必须与「实际写进 frontmatter 的值」一致：
	// 隐式类型不写 frontmatter，重启后重建指纹时读到的也是空。
	// 两边不一致会让判重只在当次进程内有效，重启即失忆。
	storedKind := kind
	if implicitKind {
		storedKind = ""
	}
	dedupKey := s.dedupKey(targetProject, writeBranch, treePrefix+"/"+storedKind, session, text)
	if prev, ok := s.dedup.Load(dedupKey); ok {
		if p, _ := prev.(string); p != "" {
			return targetProject + "/" + p, targetProject, writeBranch, nil
		}
	}

	// 纯时间戳文件名，布局由策略决定。
	// 不带 checksum：内容寻址本身就是权威判据。
	stamp := fmt.Sprintf("%s.%03d-%s.md",
		now.Format("150405"), now.Nanosecond()/1e6, endpoint)
	path := stamp
	if pol.Layout == "date" {
		path = now.Format("2006-01-02") + "/" + stamp
	}
	// 树前缀把切片放进指定子树：树路径即语义，不另设 kind 分类。
	if treePrefix = strings.Trim(treePrefix, "/"); treePrefix != "" {
		path = treePrefix + "/" + path
	}

	if err := repo.Write(writeBranch, path, []byte(content), gitstore.ModeContent); err != nil {
		return "", "", "", err
	}

	addr = targetProject + "/" + path
	s.dedup.Store(dedupKey, path)

	// 内存索引立刻更新：毫秒级可搜，不必等后台批提交。
	//
	// 键必须带分支（project@branch/path），与启动时索引重建（b.ID()）保持同构。
	// 早先传不带分支的 targetProject + "/" + path，导致刚写进去的切片在
	// recall 里拆不出分支，被当作默认分支走权限判定——如果是 chat 角色，
	// 立刻被误杀成「无权查看该分支」，直到重启重建索引才恢复。
	blockID := targetProject + "@" + writeBranch + "/" + path
	s.mu.Lock()
	s.idx.AddWithTime(blockID, text, now.Unix())
	s.addBlockLocked(targetProject, repo, writeBranch, path, int64(len(content)), now.Unix())
	s.mu.Unlock()

	s.notePending()
	s.bumpClock()

	return addr, targetProject, writeBranch, nil
}

// writeTarget 选出这次写入该落到哪个项目，并返回该项目的策略。
//
// 候选必须同时满足：
//   - 配置里声明过
//   - 这张凭证的挂载表里可见
//   - 有 w 权限
//
// 三者缺一不可。早先的实现取「s.trees 里第一个项目」，于是受限凭证
// 会被告知「项目 X 不在你的可见范围内」——而它其实有别的可写仓库。
func (s *Server) writeTarget(c Caller, persona string) (project, branch string, pol manifest.Policy, err error) {
	// 候选是「(项目, 分支)」而不是光秃秃的项目名。
	//
	// 踩过的坑：早先只按 c.Mount.DefaultBranch()（恒为 zero）判可写，
	// 而 chat 角色的挂载表里只有 chat/main/*，没有 zero 分支——
	// 于是它明明能写 daily@chat，却被告知「没有任何可写的仓库」。
	//
	// 分支不是可以默认的东西：写哪个分支，取决于挂载表怎么说。
	var cands []string
	var pairs []writeCand

	if c.Mount.Empty() {
		branch := persona
		for _, p := range s.declaredProjects() {
			if s.allowed(c, p, branch, configBitWrite) {
				cands = append(cands, p)
				pairs = append(pairs, writeCand{p, branch})
			}
		}
	} else {
		// 逐个挂载点试：项目可见 + 该分支可写，才算候选。
		for _, p := range s.declaredProjects() {
			for _, br := range c.Mount.BranchesOf(p) {
				if !c.Mount.Visible(p, br) {
					continue
				}
				if !s.allowed(c, p, br, configBitWrite) {
					continue
				}
				if len(cands) == 0 || cands[len(cands)-1] != p {
					cands = append(cands, p)
				}
				pairs = append(pairs, writeCand{p, br})
			}
		}
	}
	if len(cands) == 0 {
		// 说清楚是「看不见」还是「看得见但没 w」，否则排查时只能靠猜。
		visible := c.Mount.Projects()
		if len(visible) == 0 {
			return "", "", manifest.Policy{}, errors.New("这张凭证没有任何可写的仓库（挂载表为空，即全命名空间；请检查 [[project]] 与 mode）")
		}
		return "", "", manifest.Policy{}, fmt.Errorf(
			"这张凭证没有任何可写的仓库（可见 %s，能力 %s）",
			strings.Join(visible, " "), capOf(c))
	}

	// ① 先找声明了 stream: true 的项目——「流水归宿」由项目自己声明。
	//
	// 这是唯一不需要猜的判据：谁在 SKILL.md 里写了 stream: true，
	// 谁就是会话记录的落点。一个都没写时才往下走。
	var streams []string
	for _, p := range cands {
		if v, ok := s.policies.Load(p); ok {
			if pol := v.(manifest.Policy); pol.Stream {
				streams = append(streams, p)
			}
		}
	}
	if len(streams) == 1 {
		pol, _ := s.policyOf(streams[0])
		return streams[0], branchOf(pairs, streams[0], persona), pol, nil
	}
	if len(streams) > 1 {
		// 多个流水项目 = 意图不明。此时沉默地选一个，等于让
		// 「今天的记录写进哪个仓库」变成一次掷骰子。
		return "", "", manifest.Policy{}, fmt.Errorf(
			"有 %d 个可写项目声明了 stream: true（%s），无法判断流水该落哪里；请在 put 里显式指定目标",
			len(streams), strings.Join(streams, " "))
	}

	// ② 再找带 SKILL.md 写入声明的（「我是记忆仓库」的旧式标记）
	for _, p := range cands {
		if v, ok := s.policies.Load(p); ok {
			pol := v.(manifest.Policy)
			// SKILL.md 可以指向别的仓库，但目标必须同样可见且可写，
			// 否则回落到它自己——规则不能把写入引到凭证看不见的地方。
			br := branchOf(pairs, p, persona)
			if pol.Target != p && pol.Target != "" {
				if c.Mount.Visible(pol.Target, br) && s.allowed(c, pol.Target, br, configBitWrite) {
					return pol.Target, branchOf(pairs, pol.Target, br), pol, nil
				}
				pol.Target = p
			}
			return p, br, pol, nil
		}
	}
	return cands[0], branchOf(pairs, cands[0], persona), manifest.Default(cands[0]), nil
}

// writeCand 是一个可写的 (项目, 分支) 组合。
type writeCand struct {
	project string
	branch  string
}

// branchOf 找出某个项目在这张凭证下真正可写的分支。
//
// 不返回 Mount.DefaultBranch()：那是「人格分支」，与「这张凭证
// 被授权写哪个分支」是两件事。chat 角色的人格分支是 zero，
// 但它能写的只有 daily@chat。
func branchOf(pairs []writeCand, project, fallback string) string {
	for _, p := range pairs {
		if p.project == project {
			return p.branch
		}
	}
	return fallback
}

// policyOf 取某项目已加载的策略，没有就用默认。
func (s *Server) policyOf(project string) (manifest.Policy, bool) {
	if v, ok := s.policies.Load(project); ok {
		return v.(manifest.Policy), true
	}
	return manifest.Default(project), false
}

// resolveWriteTarget 决定写入目标。
//
// explicit 非空时必须命中它：指定了目标却写到别处，等于悄悄改主意。
// 没有权限就拒绝，绝不回落到另一个仓库——那会让「我明明写这个仓库，
// 结果写进了那个」变成一次无从察觉的事故。
func (s *Server) resolveWriteTarget(c Caller, persona, explicit string) (string, string, manifest.Policy, error) {
	if explicit == "" {
		p, br, pol, err := s.writeTarget(c, persona)
		if err != nil {
			return "", "", manifest.Policy{}, err
		}
		return p, br, pol, nil
	}

	// 挂载表优先：调用方传的是虚拟路径（例如 /daily、/me），
	// 挂载表把它翻译成真实项目和分支。
	//
	// 必须优先走它：否则挂载表定义的虚拟根没有任何意义——
	// 模型看见的是 /daily，写的时候却被逼着知道全名 blackegg/daily。
	var mountBranch string
	if !c.Mount.Empty() {
		if realProj, realBranch, _, err := c.Mount.ToReal(explicit); err == nil && realProj != "" {
			explicit = realProj
			mountBranch = realBranch
		}
	}

	project, _, ok := memory.SplitAddrWithProjects(explicit, s.declaredProjects())
	if !ok {
		// 允许直接写项目名（不带路径）
		for _, p := range s.declaredProjects() {
			if p == explicit {
				project, ok = p, true
				break
			}
		}
	}
	if !ok || project == "" {
		return "", "", manifest.Policy{}, fmt.Errorf("目标 %q 不是已声明的项目", explicit)
	}
	branch := persona
	if mountBranch != "" {
		branch = mountBranch
	} else if !c.Mount.Empty() {
		branch = c.Mount.DefaultBranch()
	}
	if !c.Mount.Visible(project, branch) {
		return "", "", manifest.Policy{}, fmt.Errorf("项目 %q 不在这张凭证的可见范围内", project)
	}
	if !s.allowed(c, project, branch, configBitWrite) {
		return "", "", manifest.Policy{}, errors.New(s.denyReason(c, project, branch, configBitWrite))
	}
	pol := manifest.Default(project)
	if v, ok := s.policies.Load(project); ok {
		pol = v.(manifest.Policy)
		if pol.Target == "" {
			pol.Target = project
		}
	}
	pol.Target = project
	return project, branch, pol, nil
}

// capOf 用一个短串描述凭证剩下多少能力，供错误信息用。
func capOf(c Caller) string {
	var out []rune
	for _, ch := range "rwx" {
		if hasCap(c.Cap, ch) {
			out = append(out, ch)
		}
	}
	if len(out) == 0 {
		return "（空）"
	}
	return string(out)
}

// policyFor 返回写入策略（兼容旧调用点，默认对全局视角）。
func (s *Server) policyFor(persona string) manifest.Policy {
	p, _, pol, err := s.writeTarget(Caller{User: "root", Root: true, Cap: "rwx"}, persona)
	if err != nil {
		return manifest.Default("daily")
	}
	_ = p
	return pol
}

// dedupKey 计算一条记忆的语义指纹。
//
// 不含 created 时间戳、不含 endpoint：同一段对话从两台机器写入，
// 或在不同的秒写入，都应该被认成同一条。session 参与计算是因为
// 同一个问题在不同会话里问两次，是两条独立的记录。
func (s *Server) dedupKey(project, branch, kind, session, text string) string {
	h := sha256.New()
	h.Write([]byte(project))
	h.Write([]byte{0})
	h.Write([]byte(branch))
	h.Write([]byte{0})
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(session))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(text)))
	return project + "@" + branch + ":" + hex.EncodeToString(h.Sum(nil))[:16]
}

// seedDedup 用已索引的块重建指纹表。
//
// 索引在启动时已经把每个块的正文读进来了，这里只是顺手再读一遍，
// 换来的效果是「服务重启后判重依然有效」——否则重启即失忆，
// 水位一丢又会重复。
func (s *Server) seedDedup(trees []*memory.Tree) {
	for _, t := range trees {
		for _, b := range t.Blocks {
			data, err := s.readBlock(b)
			if err != nil {
				continue
			}
			kind, session, text := parseSliceMeta(string(data))
			if text == "" {
				continue
			}
			key := s.dedupKey(b.Project, b.Branch, treePrefixOfPath(b.Path)+"/"+kind, session, text)
			s.dedup.Store(key, b.Path)
		}
	}
}

// parseSliceMeta 从切片正文里取回 kind / session / 正文。
//
// 只认我们自己的格式（frontmatter 后空行接正文）。不是这个格式的
// 文件（README、SKILL.md、人写的叙事）一律返回空 text，不参与判重——
// 它们本来就不会被 put 重复写入。
func parseSliceMeta(content string) (kind, session, text string) {
	if !strings.HasPrefix(content, "---\n") {
		return "", "", ""
	}
	end := strings.Index(content[4:], "\n---\n")
	if end < 0 {
		return "", "", ""
	}
	fm := content[4 : 4+end]
	body := content[4+end+5:]
	for _, line := range strings.Split(fm, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "kind":
			kind = strings.TrimSpace(v)
		case "session":
			session = strings.TrimSpace(v)
		}
	}
	text = strings.TrimSpace(body)
	if text == "" {
		return "", "", ""
	}
	return kind, session, text
}

// treePrefixOfPath 从切片路径反推它所在的子树。
//
// 切片路径形如 [<tree>/]YYYY-MM-DD/HHMMSS.mmm-endpoint.md（date 布局）
// 或 [<tree>/]HHMMSS.mmm-endpoint.md（flat 布局）。判重键必须带子树，
// 否则同一段正文写进两棵不同的树会被判成重复，只留下一条。
func treePrefixOfPath(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) == 0 {
		return ""
	}
	if len(segs) >= 2 && isDateDir(segs[len(segs)-2]) {
		return strings.Join(segs[:len(segs)-2], "/")
	}
	return strings.Join(segs[:len(segs)-1], "/")
}

// isDateDir 判断一个路径段是不是 YYYY-MM-DD。
func isDateDir(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
