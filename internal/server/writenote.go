package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/manifest"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// writeNoteGit 把一条记忆写进可写仓库的当前分支。
//
// 文件名 = 时间 + 写入序号：YYYY-MM-DD/150405.000000-12-endpoint.md。
// 不带任何 checksum 尾巴：
//   - Git 底层本身就是内容寻址（Merkle 树），每个 blob 的 SHA-1 就是它的真名；
//   - 幂等去重直接通过比较已存在条目的 Blob SHA 实现，既精准又不污染文件名；
//   - 时间戳本身就是切片的自然时间坐标，人类与机器一眼可读。
//
// 序号不是装饰：它是唯一能在同一时刻把两次写入分开的东西。
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
		return "", "", "", errors.New("refusing to write instruction or rule (inferred instructions must not enter the memory store)")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", "", errors.New("text cannot be empty")
	}

	defaultBranch := s.defaultBranch
	// 写入分支：显式挂载点优先，否则用默认分支。
	//
	// "*" 是「这个项目的所有分支」的通配声明，不是一个真实分支名。
	// 把它直接当落点会写出名为 "*" 的分支（已实测），或者在
	// 底层 ref 校验开启后直接被拒——两种都是错的：隐式写入应落到
	// 挂载点所代表的具体分支，通配时落到默认分支。
	writeBranch := defaultBranch
	if !c.Mount.Empty() {
		if b := c.Mount.DefaultBranch(); b != "" && b != "*" {
			writeBranch = b
		}
	}

	// 显式目标里写了 @<branch> 时，分支也跟着它走。
	//
	// 这是跨分支搬运的关键路径：调用方要把内容写进另一条版本线，
	// 而它的默认分支跟那条不是一回事。没有 @branch，它只能往自己
	// 分支上写，再多一步 mv——两跳之间被杀就留下半吊子状态。
	// explicitBranch 记住调用方在目标里写明的分支。
	// 它必须单独拿出来：否则后面 resolveWriteTarget 按挂载表默认分支
	// 解析时会把「写到哪条分支」这个明确意图覆盖掉（实测：
	// put proj@chat 写进了挂载表的分支，chat 根本没建）。
	explicitBranch := ""
	if explicit != "" {
		if at := strings.Index(explicit, "@"); at >= 0 {
			proj := explicit[:at]
			rest := explicit[at+1:]
			br, _, _ := strings.Cut(rest, "/")
			if br != "" {
				explicitBranch = br
				if !s.branchVisible(c, proj, br) {
					return "", "", "", fmt.Errorf("project %s@%s is outside this credential's visible scope", proj, br)
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
	//
	// 入口已经解析出明确分支时必须把它一并交给 resolveWriteTarget：
	// 那个函数会拿「默认分支」去做可写校验，而分身边界凭证的默认分支
	// 恰好是只读的 main。早先这里只传了剥掉 @branch 的项目名，于是同一
	// 次写入先在第 77 行用 chat 校验通过，紧接着在 resolveWriteTarget
	// 里用 main 再校验一次而失败——报错是 missing w permission，看起来
	// 像凭证配错，实际是分支信息在传递中丢了。
	targetProject, resolvedBranch, pol, err := s.resolveWriteTarget(c, defaultBranch, explicit, explicitBranch)
	if err != nil {
		return "", "", "", err
	}
	// 写入分支的优先级：显式 @branch > 挂载表解析出的分支 > 默认分支。
	//
	// 显式分支在入口就校验过可见性与可写性，它是最强的意图，
	// 不能被挂载表默认分支覆盖——否则「跨分支搬运」写进了错误的版本线，
	// 而返回的 ok 地址看起来还是成功的。
	if explicitBranch != "" {
		writeBranch = explicitBranch
	} else if resolvedBranch != "" {
		writeBranch = resolvedBranch
	} else if b := c.Mount.WritableBranch(targetProject); b != "" {
		// 凭证是「读主干 + 写隔离分支」（main:r + chat:rw）时，
		// 配置默认分支 main 恰好只读：不补这一层，写入永远被拒，
		// 而报错是 missing w permission，看起来像凭证配错。
		writeBranch = b
	}
	if implicitKind {
		// 落点决定类型：项目声明的第一个 kind 就是它的默认语义。
		if len(pol.Kinds) > 0 {
			kind = pol.Kinds[0]
		} else {
			kind = "fact"
		}
	} else if !pol.AllowsKind(kind) {
		return "", "", "", fmt.Errorf("kind %q is not allowed by this project's write policy (allowed: %s)",
			kind, strings.Join(pol.Kinds, " / "))
	}

	repo, err := s.projects.Ensure(targetProject)
	if err != nil {
		return "", "", "", fmt.Errorf("open write target %s: %w", targetProject, err)
	}

	now := time.Now()
	endpoint := s.cfg.Endpoint
	if endpoint == "" {
		endpoint = "local"
	}

	if source == "" {
		source = "mcp"
	}
	// 类型由落点决定：显式给了 kind 才写进 frontmatter。
	// 写一条记忆还要先声明「我是哪种」，是多出来的一步自我分类。
	kindLine := ""
	if !implicitKind {
		kindLine = fmt.Sprintf("kind: %s\n", kind)
	}
	// 落盘内容不带 session。
	//
	// 早先写 `session: <id>`，目的是「这条记忆从哪个会话来」的溯源。
	// 但它把 Pi 的压缩分支机制变成了重复源：每次压缩产生新 sessionId，
	// 而新会话文件里带着完整逐字历史（实测子文件首条消息与父文件
	// 同时间戳、同内容），重放时同一段对话被算成 N 条不同记录。
	//
	// 落盘就是对话本身。要问「这条从哪来」时，created 加内容已经够定位。
	content := fmt.Sprintf("---\n%ssource: %s\nendpoint: %s\ncreated: %s\n---\n\n%s\n",
		kindLine, source, endpoint, now.Format(time.RFC3339), text)

	// 幂等判重：按「语义内容」而不是整份文件。
	//
	// 早先的做法是把整份带 frontmatter 的 content 算 Git blob hash，
	// 但 created 时间戳每次不同——同一段文字第二次写，哈希必然不等，
	// 判重分支从未执行过。症状是「插件以为服务端会去重，服务端其实不会」，
	// 水位文件一丢就整段重复落盘。
	//
	// 现在的指纹 = kind + 正文（不含 session）。这是「这条记忆是什么」，
	// 而不是「它从哪个会话、哪个文件来」。详细论据见 dedupKey 注释。
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
	dedupKey := s.dedupKey(targetProject, writeBranch, treePrefix+"/"+storedKind, text)
	if prev, ok := s.dedup.Load(dedupKey); ok {
		if p, _ := prev.(string); p != "" {
			// 幽灵判重防护（2026-11-23 实测）：
			// 刚刚 delete 掉一批切片后，dedup 表里还缓存着它们的路径。
			// 如果此时写入一模一样的正文，直接返回那个已删路径，紧接着的
			// 回读校验（repo.Read）就会报：
			//   "write reported success but … cannot be read back"
			// 并把一次正常写入判失败。
			//
			// 命中时必须确认文件「真的还在」；已经没了的陈旧记录当场淘汰，
			// 穿透去真正写一份新的。
			rel := strings.TrimPrefix(p, targetProject+"/")
			if _, rerr := repo.Read(writeBranch, rel); rerr == nil {
				return targetProject + "/" + p, targetProject, writeBranch, nil
			}
			s.dedup.Delete(dedupKey)
		}
	}

	// 纯时间戳文件名，布局由策略决定。
	//
	// 为什么是微秒 + 序列号而不是早先的毫秒：
	// 毫秒精度（150405.000）在并发写入或批量导入时极易撞车——实测
	// 100 条并发写入会有 12 条在同一毫秒拿到完全相同的名字，后写者
	// 静默覆盖前者，且任何错误都不报。微秒把概率压低，序列号把
	// 保证做硬：同一毫秒内的并发写按单调序列号分化，绝对不重名。
	seq := s.writeSeq.Add(1)
	stamp := fmt.Sprintf("%s.%06d-%d-%s.md",
		now.Format("150405"), now.Nanosecond()/1000, seq, endpoint)
	path := stamp
	if pol.Layout == "date" {
		path = now.Format("2006-01-02") + "/" + stamp
		// 日期目录下再分一层数字槽。
		//
		// 槽号是「当天从 00:00 起的第几个 Span」，纯函数，给定时间就能算，
		// 不需要查表也不需要状态。日期目录本身是冷目录（一天最多建一次），
		// 而槽目录是热的——把热的那层压到几十个文件，写一次改的是
		// 一点几 KB 的 tree，而不是几千条目的 163 KB。
		if pol.Span > 0 {
			dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			slot := int(now.Sub(dayStart) / pol.Span)
			maxSlot := int(24*time.Hour/pol.Span) - 1
			if maxSlot < 0 {
				maxSlot = 0
			}
			if slot > maxSlot {
				// 夏令时把一天拉长到 25 小时时会出现；夹到最后一个槽，
				// 绝不产生越界槽号（那会让路径随时间倒退）。
				slot = maxSlot
			}
			path = now.Format("2006-01-02") + "/" + fmt.Sprintf("%03d", slot) + "/" + stamp
		}
	}
	// 树前缀把切片放进指定子树：树路径即语义，不另设 kind 分类。
	if treePrefix = strings.Trim(treePrefix, "/"); treePrefix != "" {
		path = treePrefix + "/" + path
	}

	// 记下写入前的头，供事件携带——「写入发生在哪个版本之上」。
	prevHead, _ := repo.Resolve(writeBranch)

	if err := repo.Write(writeBranch, path, []byte(content), gitstore.ModeContent); err != nil {
		return "", "", "", err
	}

	// 写入即通知：commit 是底层折叠动作，不是语义事件。
	//
	// 早先广播只挂在 commit 上，于是「外部往 inbox 写一条，等它自己折叠」
	// 这条最常见的用法根本不触发唤醒——数据进了脏层，索引也更新了，
	// 但订阅者收不到任何东西。让调用方为了叫醒别人而多调一次 commit，
	// 等于把内部机制泄漏成外部契约。
	//
	// 语义从「树被折叠了」改成「树被写了」。commit 留空：脏层没有新版本号，
	// path 必须带上：客户端据此知道「这一次写的是哪个文件」。早先传空，
	// 订阅方只能 ls 一遍猜最新一条，猜错就反复处理同一条旧输入。
	// 取差量的正确方式是按路径读，而不是 since <commit>。
	s.publishTreeChange(targetProject, writeBranch, path, prevHead, "", EventOrigin{
		Identity: c.Identity,
		Session:  c.Session,
		Endpoint: s.cfg.Endpoint,
	})

	addr = targetProject + "/" + path
	s.dedup.Store(dedupKey, path)

	// 内存索引立刻更新：毫秒级可搜，不必等后台批提交。
	//
	// 键必须带分支（project@branch/path），与启动时索引重建（b.ID()）保持同构。
	// 早先传不带分支的 targetProject + "/" + path，导致刚写进去的切片在
	// recall 里拆不出分支，被当作默认分支走权限判定——如果是受限凭证，
	// 立刻被误杀成「无权查看该分支」，直到重启重建索引才恢复。
	//
	// 正文也必须与重建时一致：重建走的是 readBlock + 整份文件（含
	// frontmatter），这里早先只索引 text，于是 source/endpoint/session
	// 这些词在重启前搜不到、重启后才出现——同一份记忆的可检索性
	// 随进程生命周期变化，docLen 也跟着变，BM25 分数跟着漂。
	blockID := memory.MakeID(targetProject, writeBranch, path)
	s.idxNow().AddWithTime(blockID, content, now.Unix())
	s.addBlockLocked(targetProject, repo, writeBranch, path, int64(len(content)), now.Unix())

	s.bumpClock()
	// 写入改变了树状态：请求一次语义索引追平。
	// 非阻塞、去重（3 秒窗口攒批），失败由后台周期兜底重试。
	s.kickSemantic()

	return addr, targetProject, writeBranch, nil
}

// writeTarget 选出这次写入该落到哪个项目，并返回该项目的策略。
//
// 候选必须同时满足：
//   - 配置里声明过
//   - 这张凭证的挂载表里可见
//   - 有 w 权限
//
// 三者缺一不可。早先的实现取「s.treeList() 里第一个项目」，于是受限凭证
// 会被告知「项目 X 不在你的可见范围内」——而它其实有别的可写仓库。
func (s *Server) writeTarget(c Caller, defaultBranch string) (project, branch string, pol manifest.Policy, err error) {
	// 候选是「(项目, 分支)」而不是光秃秃的项目名。
	//
	// 踩过的坑：早先只按 c.Mount.DefaultBranch()（恒为 zero）判可写，
	// 而它的挂载表里只有自己那几条分支——于是它明明能写，
	// 却被告知「没有任何可写的仓库」。
	//
	// 分支不是可以默认的东西：写哪个分支，取决于挂载表怎么说。
	var cands []string
	var pairs []writeCand

	if c.Mount.Empty() {
		branch := defaultBranch
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
				if !s.branchVisible(c, p, br) {
					continue
				}
				if !s.allowed(c, p, br, configBitWrite) {
					continue
				}
				if len(cands) == 0 || cands[len(cands)-1] != p {
					cands = append(cands, p)
				}
				if br == "*" {
					// 通配挂载的隐式落点是默认分支，不是字面的 "*"。
					// 显式 put proj@branch 在更前面已经处理过，不经过这里。
					br = defaultBranch
				}
				pairs = append(pairs, writeCand{p, br})
			}
		}
	}
	if len(cands) == 0 {
		// 说清楚是「看不见」还是「看得见但没 w」，否则排查时只能靠猜。
		visible := c.Mount.Projects()
		if len(visible) == 0 {
			return "", "", manifest.Policy{}, errors.New("this credential has no writable repositories (an empty mount table means the entire namespace; check [[project]] and mode)")
		}
		return "", "", manifest.Policy{}, fmt.Errorf(
			"this credential has no writable repositories (visible: %s, capabilities: %s)",
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
		return streams[0], branchOf(pairs, streams[0], defaultBranch), pol, nil
	}
	if len(streams) > 1 {
		// 多个流水项目 = 意图不明。此时沉默地选一个，等于让
		// 「今天的记录写进哪个仓库」变成一次掷骰子。
		return "", "", manifest.Policy{}, fmt.Errorf(
			"%d writable projects declare stream: true (%s); the stream destination is ambiguous. Specify a target in put",
			len(streams), strings.Join(streams, " "))
	}

	// ② 再找带 SKILL.md 写入声明的（「我是记忆仓库」的旧式标记）
	for _, p := range cands {
		if v, ok := s.policies.Load(p); ok {
			pol := v.(manifest.Policy)
			// SKILL.md 可以指向别的仓库，但目标必须同样可见且可写，
			// 否则回落到它自己——规则不能把写入引到凭证看不见的地方。
			br := branchOf(pairs, p, defaultBranch)
			if pol.Target != p && pol.Target != "" {
				if c.Mount.Visible(pol.Target, br) && s.allowed(c, pol.Target, br, configBitWrite) {
					return pol.Target, branchOf(pairs, pol.Target, br), pol, nil
				}
				pol.Target = p
			}
			return p, br, pol, nil
		}
	}
	return cands[0], branchOf(pairs, cands[0], defaultBranch), manifest.Default(cands[0]), nil
}

// writeCand 是一个可写的 (项目, 分支) 组合。
type writeCand struct {
	project string
	branch  string
}

// branchOf 找出某个项目在这张凭证下真正可写的分支。
//
// 不返回 Mount.DefaultBranch()：那是「默认分支」，与「这张凭证
// 被授权写哪个分支」是两件事。受限凭证的默认分支是 zero，
// 但它能写的只有挂载表里那条。
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
func (s *Server) resolveWriteTarget(c Caller, defaultBranch, explicit string, preferredBranch ...string) (string, string, manifest.Policy, error) {
	pref := ""
	if len(preferredBranch) > 0 && preferredBranch[0] != "" {
		pref = preferredBranch[0]
	}
	if explicit == "" {
		p, br, pol, err := s.writeTarget(c, defaultBranch)
		if err != nil {
			return "", "", manifest.Policy{}, err
		}
		if pref != "" {
			br = pref
		}
		return p, br, pol, nil
	}

	// 挂载表优先：调用方传的是虚拟路径，
	// 挂载表把它翻译成真实项目和分支。
	//
	// 必须优先走它：否则挂载表定义的虚拟根没有任何意义——
	// 调用方看见的是虚拟路径，写的时候却被逼着知道真实项目全名。
	// 但**只在 explicit 不是真实项目名时**才走它。
	//
	// 踩过的坑：显式写了 BlackEggAI/weft@main.record，挂载表里却有
	// 多条根在 "/" 的挂载点，ToReal 按第一条命中返回，于是这次写入
	// 被翻成了 BlackEggAI/zero@main.record——内容和地址都"成功"，
	// 只是进错了仓库。反过来（虚拟路径 → 真实项目）是它的正当用途，
	// 所以判定条件只能是「explicit 本身是不是已声明的项目」。
	var mountBranch string
	if !c.Mount.Empty() && !s.namesDeclaredProject(explicit) {
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
		return "", "", manifest.Policy{}, fmt.Errorf("target %q is not a declared project", explicit)
	}
	branch := defaultBranch
	if pref != "" {
		branch = pref
	} else if mountBranch != "" {
		branch = mountBranch
	} else if wb := c.Mount.WritableBranch(project); wb != "" {
		// 挂载表定义了对该项目的可写分支时优先落入，
		// 避免「main:r + chat:rw」被配置默认分支 main 抢占而报只读错误。
		branch = wb
	} else if !c.Mount.Empty() {
		branch = c.Mount.DefaultBranch()
	}
	// 可见性以「挂载表 或 服务端授权表」为准：ACL 开出来的分支
	// 不在凭证里，但它确实对这张凭证可见。
	if !s.branchVisible(c, project, branch) {
		return "", "", manifest.Policy{}, fmt.Errorf("project %q is outside this credential's visible scope", project)
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
		return "(empty)"
	}
	return string(out)
}

// dedupKey 计算一条记忆的语义指纹。
//
// 不含 created 时间戳、不含 endpoint：同一段对话从两台机器写入，
// 或在不同的秒写入，都应该被认成同一条。
//
// 也不含 session。这一条是走了一大圈才改对的（2026-11-23）：
//
// 早先的注释写的是「session 参与计算是因为同一个问题在不同会话里
// 问两次，是两条独立的记录」。单看这句话合理，但它遇上了 Pi 的
// 压缩分支机制就变成灾难：压缩会写出新会话文件，新文件里带着
// **完整逐字历史**（实测子文件首条消息与父文件同时间戳、同内容，
// 两份文件内容重合 99.3%）。重放这份历史时 session 一换，
// 指纹就全变了，于是同一段对话被当成全新记录再写一遍。
//
// 实测一条链 28 个会话：累计回答 80,290 轮，去重后只有 6,498 轮，
// 重复占 91.9%。存储里同一句「读一下这个项目」出现过 24 份。
//
// 根因不是重放，而是「同一段内容因为出处不同而被当成两条」。
// 两条记录问的是同一句话、写的是同一个答案，它就是同一条记忆。
// 出处（哪个会话、哪台机器）不属于这条记忆的身份。
func (s *Server) dedupKey(project, branch, kind, text string) string {
	h := sha256.New()
	h.Write([]byte(project))
	h.Write([]byte{0})
	h.Write([]byte(branch))
	h.Write([]byte{0})
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(text)))
	return project + "@" + branch + ":" + hex.EncodeToString(h.Sum(nil))[:16]
}

// seedDedup 用已索引的块重建指纹表。
//
// 索引在启动时已经把每个块的正文读进来了，这里只是顺手再读一遍，
// 换来的效果是「服务重启后判重依然有效」——否则重启即失忆，
// 水位一丢又会重复。
// seedDedup 从内存树里挑出「最近」的切片预热判重表。
//
// 为什么只读最近的：判重表的唯一用途是「防同一轮写入重复投递」
// （网络重试、或者同一个提示词分词后被两个 worker 同时塞进来）。
// 这种重复永远发生在**当下**（同一分钟内）。
//
// 早先对全库 25,000 块逐块读盘——实测 47 秒，整整占了快照启动
// 耗时的 95%！读 2026 年初的历史切片去防今天的重复投递，
// 毫无意义。
//
// 现在只读「路径里带今天、昨天或前天日期」的切片，或没有日期的文件。
// 25,000 块降到几十块，耗时从 47 秒变成几毫秒。
func (s *Server) seedDedup(trees []*memory.Tree) {
	cutoff := time.Now().Add(-72 * time.Hour).Format("2006-01-02")

	// 只收最近 72 小时内的切片：更早的历史不可能与今天的写入碰撞。
	var cands []memory.Block
	for _, t := range trees {
		for _, b := range t.Blocks {
			if d := dateInPath(b.Path); d != "" && d < cutoff {
				continue
			}
			cands = append(cands, b)
		}
	}
	if len(cands) == 0 {
		return
	}

	// 并行读盘。
	//
	// 为什么必须并行：热启动时 Block 来自快照，Body 是 nil，这里每一篇
	// 都要真正解压一次。实测 5,000 篇串行读盘 9.2 秒——而快照存在的
	// 全部意义就是让启动更快，结果比冷建（6.1 秒）还慢 50%。
	// 这些读彼此完全独立，串行做没有任何理由。
	type seeded struct {
		key  string
		path string
	}
	out := make([]seeded, len(cands))
	workers := runtime.NumCPU() * 4
	if workers > 32 {
		workers = 32
	}
	if workers > len(cands) {
		workers = len(cands)
	}
	var wg sync.WaitGroup
	idx := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				b := cands[i]
				data, err := s.readBlock(b)
				if err != nil {
					continue
				}
				kind, text := parseSliceMeta(string(data))
				if text == "" {
					continue
				}
				out[i] = seeded{
					key:  s.dedupKey(b.Project, b.Branch, treePrefixOfPath(b.Path)+"/"+kind, text),
					path: b.Path,
				}
			}
		}()
	}
	for i := range cands {
		idx <- i
	}
	close(idx)
	wg.Wait()

	// 单线程写入去重表：sync.Map 本身并发安全，但保持写入顺序让
	// 「同一 key 多条命中时保留哪一条」可复现。
	for _, sd := range out {
		if sd.key != "" {
			s.dedup.Store(sd.key, sd.path)
		}
	}
}

// dateInPath 从路径里抽出 YYYY-MM-DD 形状的日期段。找不到返回空。
func dateInPath(p string) string {
	for _, seg := range strings.Split(p, "/") {
		if len(seg) == 10 && seg[4] == '-' && seg[7] == '-' {
			// 粗判是否全数字
			ok := true
			for i, c := range seg {
				if (i == 4 || i == 7) && c == '-' {
					continue
				}
				if c < '0' || c > '9' {
					ok = false
					break
				}
			}
			if ok {
				return seg
			}
		}
	}
	return ""
}

// parseSliceMeta 从切片正文里取回 kind / 正文。
//
// 只认我们自己的格式（frontmatter 后空行接正文）。不是这个格式的
// 文件（README、SKILL.md、人写的叙事）一律返回空 text，不参与判重——
// 它们本来就不会被 put 重复写入。
//
// 早先还额外返回 session，供指纹使用。现在指纹不看 session，
// 也就不必从历史切片里把它读回来——旧切片里可能还带着 session 行，
// 没有任何影响，就当普通 frontmatter 跳过。
func parseSliceMeta(content string) (kind, text string) {
	if !strings.HasPrefix(content, "---\n") {
		return "", ""
	}
	end := strings.Index(content[4:], "\n---\n")
	if end < 0 {
		return "", ""
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
		}
	}
	text = strings.TrimSpace(body)
	if text == "" {
		return "", ""
	}
	return kind, text
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
