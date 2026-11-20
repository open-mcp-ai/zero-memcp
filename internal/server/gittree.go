package server

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/manifest"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// loadGitTrees 按分支扫描所有项目，构建内存树。
func (s *Server) loadGitTrees(defaultBranch string) ([]*memory.Tree, error) {
	projects, err := s.projects.Projects()
	if err != nil {
		return nil, err
	}
	startupStage("reload: %d project(s): %v", len(projects), projects)

	var trees []*memory.Tree
	for _, proj := range projects {
		repo, err := s.projects.Get(proj)
		if err != nil {
			return nil, fmt.Errorf("open project %s: %w", proj, err)
		}

		// 索引所有分支，不只 defaultBranch。
		//
		// 为什么必须这样：权限是按分支划分的。只索引 defaultBranch 的话，
		// 受限身份写进其他分支的内容永远搜不到——写成功、read 读得到、
		// recall 无命中，一个不报错的静默失效。
		//
		// 索引是纯内存派生物，多扫几个分支的代价只是启动时多几百毫秒。
		branches := repo.Branches()
		if len(branches) == 0 {
			branches = []string{defaultBranch}
		}
		// 把 defaultBranch 和 pending 里的分支也带上（可能是还没提交的新分支）
		seen := map[string]bool{}
		for _, b := range branches {
			seen[b] = true
		}
		if !seen[defaultBranch] {
			branches = append(branches, defaultBranch)
			seen[defaultBranch] = true
		}
		for _, b := range repo.PendingRefs() {
			if !seen[b] {
				branches = append(branches, b)
				seen[b] = true
			}
		}

		for _, br := range branches {
			has, _ := repo.Resolve(br)
			if has == "" && repo.Pending(br) == 0 {
				continue
			}
			startupStage("reload: scanning %s@%s (head %s)", proj, br, shortSHA(has))
			t, err := s.scanGitProject(repo, proj, br)
			if err != nil {
				return nil, fmt.Errorf("scan %s/%s: %w", proj, br, err)
			}
			startupStage("reload: %s@%s has %d entries", proj, br, len(t.Blocks))
			trees = append(trees, t)
		}
	}
	return trees, nil
}

// scanGitProject 把一个仓库的一个分支读成内存树。
func (s *Server) scanGitProject(repo *gitstore.Repo, project, defaultBranch string) (*memory.Tree, error) {
	entries, err := repo.List(defaultBranch)
	if err != nil {
		return nil, err
	}
	startupStage("reload: %s@%s listed %d paths", project, defaultBranch, len(entries))

	t := &memory.Tree{Project: project, Branch: defaultBranch}
	t.Blocks = s.makeBlocksParallel(repo, project, defaultBranch, entries)
	// 规则目录的兄弟文件也是规则。
	//
	// 判据来自 SKILL.md 自己写的那句「按文件名顺序读就是全文」——
	// 也就是说 01-rebuilt.md、02-shape.md … 和 SKILL.md 合起来才是
	// 一份完整的规则集。只注入目录索引而不注入正文，等于给了目录
	// 却不给内容：模型知道"有 6 个文件"，但一个字都看不到。
	ruleDirs := map[string]bool{}
	for _, b := range t.Blocks {
		if b.Rule {
			if i := strings.LastIndex(b.Path, "/"); i > 0 {
				ruleDirs[b.Path[:i]] = true
			}
		}
	}
	if len(ruleDirs) > 0 {
		for i := range t.Blocks {
			b := &t.Blocks[i]
			if b.Rule || b.Hidden || !strings.HasSuffix(b.Path, ".md") {
				continue
			}
			dir := b.Path
			if j := strings.LastIndex(dir, "/"); j > 0 {
				dir = dir[:j]
			} else {
				dir = ""
			}
			if ruleDirs[dir] {
				b.Rule = true
			}
		}
	}

	t.Sort()
	return t, nil
}

// hasFrontmatter 判断文件是否以 YAML frontmatter 开头。
func hasFrontmatter(data []byte) bool {
	return strings.HasPrefix(string(data), "---")
}

// isExecutableScript 判断是不是脚本/工具类文件。
func isExecutableScript(path string) bool {
	for _, ext := range []string{".sh", ".py", ".bash", ".zsh", ".js", ".mjs", ".ts", ".rb", ".pl"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// addBlockLocked 把一条刚写入的切片加进内存树，让它毫秒级可搜。
//
// 调用方必须已持有 s.mu 写锁。
//
// 为什么不用 Reload()：Reload 要重扫全部仓库（2286 个文件、四万多次
// tree 解析）。写一条记忆触发一次全量重建，写入就会变成秒级操作，
// 而写入是热路径。
func (s *Server) addBlockLocked(project string, repo *gitstore.Repo, branch, path string, size int64, mtime int64) {
	idx := s.idxNow()
	if idx == nil {
		return
	}
	release, err := s.acquireBranchWrite(project, branch)
	if err != nil {
		return
	}
	defer release()

	r := repo
	read := func() ([]byte, error) { return r.Read(branch, path) }

	// blob hash 必须一起更新：后台语义追平靠它判断「内容变没变」，
	// 不更新的话那个块会被永久当成「已嵌入、没变化」——
	// 症状是「写了新内容，语义搜索还是旧向量」。
	var hash string
	if e, ok, serr := repo.Stat(branch, path); serr == nil && ok {
		hash = e.Hash
	}

	cur := s.treeList()
	next := cloneTreeList(cur)

	// 已发布的树不可修改：先 clone 那一棵，改副本。
	ti := findTree(next, project, branch)
	var tree *memory.Tree
	if ti < 0 {
		tree = &memory.Tree{Project: project, Branch: branch}
		next = append(next, tree)
	} else {
		tree = cloneTree(next[ti])
		next[ti] = tree
	}

	// 同路径覆盖（同一毫秒内重写同一文件时不会产生重复条目）
	replaced := false
	for i := range tree.Blocks {
		if tree.Blocks[i].Path == path {
			tree.Blocks[i].Size = size
			tree.Blocks[i].Time = mtime
			tree.Blocks[i].Hash = hash
			replaced = true
			break
		}
	}
	if !replaced {
		tree.Blocks = append(tree.Blocks, memory.Block{
			Project: project,
			Branch:  branch,
			Path:    path,
			Size:    size,
			Time:    mtime,
			Hash:    hash,
			Read:    read,
		})
		tree.Sort()
	}
	s.publishTrees(next, idx)
}

// indexedPaths 返回内存树里某条分支的全部路径（副本）。
//
// 用于「只要路径」的场景（如 trees 列目录）。索引里没有这条分支时
// 返回 nil，调用方据此退回真扫一次仓库。
func (s *Server) indexedPaths(project, branch string) []string {
	for _, t := range s.treeList() {
		if t.Project != project || t.Branch != branch {
			continue
		}
		out := make([]string, len(t.Blocks))
		for i, b := range t.Blocks {
			out[i] = b.Path
		}
		return out
	}
	return nil
}

// makeBlocksParallel 并行把一批 git 条目变成内存块。
//
// 为什么必须并行：每个 .md 都要读出来判 frontmatter，而读的是 zlib
// 解压 + pack 里找对象——纯 I/O + CPU，彼此完全独立。串行跑在
// 25,000 个文件上实测 65 秒，全部摊在发版那段不可用的窗口里。
//
// 分段的实测数字（v19）：扫描 65.2s、索引 7.7s（其中读盘 0.0s）。
// 也就是说**慢的是扫描，不是分词**——所以并行化加在这里，
// 而不是加在 AddWithTime 上。
//
// 顺序必须保持不变：Blocks 的顺序影响树的 Sort 稳定性与日志可读性，
// 也让「同样的输入产出同样的树」这件事成立（对拍测试依赖它）。
func (s *Server) makeBlocksParallel(repo *gitstore.Repo, project, branch string, entries []gitstore.Entry) []memory.Block {
	out := make([]memory.Block, len(entries))
	if len(entries) == 0 {
		return out
	}

	// 并发度：扫描是 I/O 密集（读 pack 里的对象），给足并发。
	// 上限 16 是为了不让机械盘/网络盘出现抖动反而更慢。
	workers := runtime.NumCPU() * 4
	if workers > 16 {
		workers = 16
	}
	if workers > len(entries) {
		workers = len(entries)
	}

	var wg sync.WaitGroup
	idx := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				e := entries[i]
				path := e.Path
				r := repo
				read := func() ([]byte, error) {
					return r.Read(branch, path)
				}
				out[i] = s.makeBlock(project, branch, e, read)
			}
		}()
	}
	for i := range entries {
		idx <- i
	}
	close(idx)
	wg.Wait()
	return out
}

// makeBlock 把一个 git 条目变成内存块，并就地判出 Rule / Hidden。
//
// 抽出来是为了 `write` / `delete` 能只更新一个 path——
// 早先它们调 refreshBranch，整条分支 5233 个文件全部重读重分词，
// 一次 write 要 19 秒。而它们真正改变的只有一个文件。
func (s *Server) makeBlock(project, branch string, e gitstore.Entry, read func() ([]byte, error)) memory.Block {
	block := memory.Block{
		Project: project,
		Branch:  branch,
		Path:    e.Path,
		Size:    e.Size,
		Time:    e.Time,
		Hash:    e.Hash, // 免读盘判断内容变没变，见 Block.Hash
		Read:    read,
	}

	data, rerr := read()
	if rerr == nil {
		// 只读一次：判定要用，索引阶段也要用。
		//
		// 早先这里读一次判 frontmatter，索引阶段 readBlock 再读一次——
		// 同一个文件读两遍。库里 24,000 块绝大多数是 .md，
		// 启动时这一倍的白读直接摊在不可用窗口里。
		block.Body = data
	}
	_, rule, hidden := classifyContent(project, e.Path, data, e.Mode)
	block.Rule = rule
	block.Hidden = hidden
	// 顺手把「内容自己声明的层」解析出来。
	// 这里 content 已经在手上，多解析一个字段是零成本的；
	// 放到索引阶段去做就要多读一次盘。
	if rerr == nil {
		block.Layer = memory.DeclaredLayer(string(data))
	}
	return block
}

// classifyContent 判定一份内容是不是规则、要不要进检索。
//
// 抽成独立函数是因为有三条路径要做同一件事：全量扫描、单文件增量刷新、
// 以及启动时应用 git 差量。三处各写一份判定，慢慢就会走偏——
// 而走偏的症状是「规则不注入了」或「该搜的搜不到」，都不报错。
//
// 判据优先看文件自己写的 frontmatter，而不是 Git 的可执行位：
// 100755 在真实仓库里的含义是「这是个可执行脚本」。用它当「规则」标记，
// 后果是 .githooks/pre-push 和一个 11KB 的 python 脚本被当成规则
// 注入系统提示，而真正的 SKILL.md（全是 100644）一个都没进。
// 文件自己写着 inject: every_turn —— 那才是作者的本意。
func classifyContent(project, path string, data []byte, mode gitstore.Mode) (pol manifest.Policy, rule, hidden bool) {
	// 以点开头的目录/文件（.legacy-archive、.githooks、.gitignore）
	// 是归档与工具，不是正文：保留可读，但不进检索。
	if memory.HiddenPath(path) {
		return pol, false, true
	}

	if !strings.HasSuffix(path, ".md") {
		// 非 Markdown 一律不是规则。
		// 可执行脚本再「可执行」也只是工具，不是给模型的指令。
		return pol, false, isExecutableScript(path)
	}

	if data != nil && hasFrontmatter(data) {
		pol = manifest.Parse(project, data)
		switch pol.Inject {
		case "every_turn":
			rule = true
		case "never":
			rule = false
		default:
			// 没声明 inject：说明作者只写了 write 策略，
			// 这时仍按 Git 的可执行位判断，保持旧行为。
			rule = mode == gitstore.ModeRule
		}
		return pol, rule, pol.Index == "none"
	}
	// 没有 frontmatter 的 Markdown：按老规矩看 mode 位
	return pol, mode == gitstore.ModeRule, false
}

// refreshPath 只更新一条分支上的一个文件：树里的 Block 和索引里的 doc。
//
// 为什么需要它：write / delete 早先调 refreshBranch —— 那条路会把整条
// 分支重扫一遍（List 全部路径、每个 .md 读两次、全部重新分词）。
// main.chat 有 5233 个文件，一次 write 实测 19 秒、一次 delete 18 秒，
// 而它们真正改变的是一个文件。规模越大越慢，正是「越用越卡」的来源。
//
// 语义与 refreshBranch 等价：那边是「撤销整分支 → 全量加回去」，
// 净效果就是让索引反映当前磁盘状态。这里对单个 path 做同一件事。
//
// exists=false 表示这个文件已经从树里消失（delete）。
func (s *Server) refreshPath(project, branch, path string, exists bool) error {
	repo, err := s.projects.Get(project)
	if err != nil {
		return fmt.Errorf("open project %s: %w", project, err)
	}

	id := memory.MakeID(project, branch, path)

	// 先把新块算出来（读盘在锁外：不能在持锁时做 I/O）。
	var (
		block memory.Block
		text  string
		stamp int64
	)
	if exists {
		// Stat 只走路径深度（O(dirs)），不展开整条分支。
		entry, found, serr := repo.Stat(branch, path)
		if serr != nil {
			return fmt.Errorf("stat %s@%s:%s: %w", project, branch, path, serr)
		}
		if !found {
			exists = false
		} else {
			r := repo
			read := func() ([]byte, error) { return r.Read(branch, path) }
			block = s.makeBlock(project, branch, entry, read)
			if data, rerr := read(); rerr == nil {
				text = string(data)
				stamp = entry.Time
				if at := sliceAtTime(text, path); at > 0 {
					stamp = at
				}
			}
		}
	}

	release, lerr := s.acquireBranchWrite(project, branch)
	if lerr != nil {
		return lerr
	}
	defer release()

	idx := s.idxNow()
	// ① 索引：内部无锁，直接增删。
	if idx != nil {
		idx.Remove(id)
		if exists && !block.Hidden && text != "" {
			idx.AddWithTime(id, text, stamp)
		}
	}

	// ② 树：不可变发布。先复制树列表，再克隆目标树并修改副本。
	cur := s.treeList()
	next := cloneTreeList(cur)
	ti := findTree(next, project, branch)
	var tree *memory.Tree
	if ti < 0 {
		if !exists {
			return nil
		}
		tree = &memory.Tree{Project: project, Branch: branch}
		next = append(next, tree)
	} else {
		tree = cloneTree(next[ti])
		next[ti] = tree
	}

	replaced := false
	for i := range tree.Blocks {
		if tree.Blocks[i].Path != path {
			continue
		}
		if exists {
			block.Body = nil // 只在这一轮有用，不留进长期树
			tree.Blocks[i] = block
		} else {
			tree.Blocks = append(tree.Blocks[:i], tree.Blocks[i+1:]...)
		}
		replaced = true
		break
	}
	if !replaced && exists {
		block.Body = nil
		tree.Blocks = append(tree.Blocks, block)
		tree.Sort()
	}
	s.publishTrees(next, idx)

	// ③ SKILL.md 变了要重解析策略：它决定「谁能看见什么」。
	if exists && (path == "SKILL.md" || strings.HasSuffix(path, "/SKILL.md")) {
		if data, rerr := block.Read(); rerr == nil {
			s.policies.Store(project, manifest.Parse(project, data))
		}
	}
	return nil
}

// refreshBranch 只重建一条分支的内存树与索引条目。
//
// 为什么需要它：Reload() 重扫全部仓库、逐块读盘、重建整个倒排索引
// （21k 块、数十秒）。而它被挂在每一次 git push 上——一次外部推进
// 就让整个服务几十秒不可用，症状是「命令偶发不通，过一会儿自己好了」。
//
// 这里只做那条分支：扫它、换掉它的树、把它的 doc 从索引里精确撤销
// 再重新加进去。别的分支一个字节都不动。
//
// 出错时返回错误，调用方应当退回 Reload()——安全的那条路永远留着。
func (s *Server) refreshBranch(project, branch string) error {
	repo, err := s.projects.Get(project)
	if err != nil {
		return fmt.Errorf("open project %s: %w", project, err)
	}
	tree, err := s.scanGitProject(repo, project, branch)
	if err != nil {
		return fmt.Errorf("scan %s@%s: %w", project, branch, err)
	}

	// SKILL.md 的策略在扫描时已经解析过，这里只需要把可能变化的那份换上去。
	for _, b := range tree.Blocks {
		if b.Path == "SKILL.md" || strings.HasSuffix(b.Path, "/SKILL.md") {
			if data, rerr := s.readBlock(b); rerr == nil {
				s.policies.Store(tree.Project, manifest.Parse(tree.Project, data))
			}
		}
	}

	release, lerr := s.acquireBranchWrite(project, branch)
	if lerr != nil {
		return lerr
	}
	defer release()

	idx := s.idxNow()
	if idx != nil {
		// 先撤销旧分支的全部 doc，再按新树加回去。
		for _, old := range s.treeList() {
			if old.Project != project || old.Branch != branch {
				continue
			}
			for _, blk := range old.Indexable() {
				idx.Remove(blk.ID())
			}
		}
		for _, blk := range tree.Indexable() {
			data, rerr := s.readBlock(blk)
			if rerr != nil {
				continue
			}
			stamp := blk.Time
			if at := sliceAtTime(string(data), blk.Path); at > 0 {
				stamp = at
			}
			idx.AddWithTime(blk.ID(), string(data), stamp)
		}
	}
	// 索引建完就扔掉扫描期缓存：留着等于整分支正文常驻内存。
	for i := range tree.Blocks {
		tree.Blocks[i].Body = nil
	}

	cur := s.treeList()
	next := cloneTreeList(cur)
	if ti := findTree(next, project, branch); ti < 0 {
		next = append(next, tree)
	} else {
		next[ti] = tree
	}
	s.publishTrees(next, idx)
	return nil
}

// refreshBranches 批量刷新若干条分支，任一失败就整体退回全量 Reload。
//
// 退回是刻意的：增量刷新是优化，全量重建是正确性保底。增量出问题时
// 宁可慢，也不能让索引与实际仓库不一致——那种不一致的表现是
// 「写进去了但搜不到」，而它不会报任何错。
func (s *Server) refreshBranches(project string, branches []string, reason string) {
	if len(branches) == 0 {
		return
	}
	for _, br := range branches {
		if err := s.refreshBranch(project, br); err != nil {
			s.logf("%s: incremental refresh of %s@%s failed (%v); falling back to full reload",
				reason, project, br, err)
			if rerr := s.Reload(); rerr != nil {
				s.logf("%s: full reload also failed: %v", reason, rerr)
			}
			return
		}
	}
	s.logf("%s: refreshed %d branch(es) of %s incrementally (%s)",
		reason, len(branches), project, strings.Join(branches, ", "))
}

// dropBranch 从内存树与索引里摘掉一条分支（分支被归档/删除时用）。
//
// 为什么不能只 refreshBranch：归档之后源分支在 refs 里已经不存在，
// 而索引里它的 doc 还在——症状是「归档了但还能 recall 到」。
func (s *Server) dropBranch(project, branch string) {
	release, err := s.acquireBranchWrite(project, branch)
	if err != nil {
		return
	}
	defer release()

	cur := s.treeList()
	ti := findTree(cur, project, branch)
	if ti < 0 {
		return
	}
	idx := s.idxNow()
	if idx != nil {
		for _, b := range cur[ti].Indexable() {
			idx.Remove(b.ID())
		}
	}
	next := make([]*memory.Tree, 0, len(cur)-1)
	next = append(next, cur[:ti]...)
	next = append(next, cur[ti+1:]...)
	s.publishTrees(next, idx)
}

// allIndexableBlocks 返回全库可索引块的副本（测试与诊断用）。
func (s *Server) allIndexableBlocks() []memory.Block {
	var out []memory.Block
	for _, t := range s.treeList() {
		out = append(out, t.Indexable()...)
	}
	return out
}
