package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/index"
	"github.com/open-mcp-ai/zero-memcp/internal/manifest"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// indexSnapshotDir 是索引快照的落盘目录。
func (s *Server) indexSnapshotDir() string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, "system", "indexsnap")
}

// tryLoadIndexSnapshot 尝试用落盘快照恢复索引。
//
// 返回 true 表示已成功恢复，调用方不必再全量扫描。
//
// 判据（两条同时成立才恢复）：
//  1. 头部能读、magic/version 对得上
//  2. 反序列化出来的二进制能读通（结构自洽、校验和对）
//
// 文档数不一致**不再**是拒收理由，见下面 healIndexAgainstTrees。
// 这条「宁可慢也不给错」的边界仍然是刻意的：错的索引不会报错，
// 只会让「写了但搜不到」，那种故障比启动慢难查一百倍。
// 所以治愈的方式是「与树逐条对齐」，而不是「假装没看见」。
func (s *Server) tryLoadIndexSnapshot(defaultBranch string) bool {
	dir := s.indexSnapshotDir()
	if dir == "" || os.Getenv("ZERO_MEMCP_NO_INDEX_SNAPSHOT") != "" {
		return false
	}

	t0 := time.Now()
	hdr, err := index.LoadSnapshotHeader(dir)
	if err != nil {
		startupStage("reload: no usable index snapshot (%v); full rebuild", err)
		return false
	}

	// 树从树快照恢复，跳过扫描。
	//
	// 这是启动加速的大头：扫描 23s、分词 9s，只存倒排表只能省后者。
	// 而树的每个 Block 除 Read 之外全是值，Read 就是
	// repo.Read(branch, path)——可以从路径重建。
	//
	// 树快照读不出来就退回真扫（慢 23 秒，但仍然正确）。
	// 不能只加载倒排表而没有 Blocks：那样 grep/read 直接失灵，
	// 而 recall 看起来正常——半残状态最难查。
	trees, terr := s.restoreTreesFromSnapshot(defaultBranch)
	if terr != nil {
		startupStage("reload: tree snapshot unusable (%v); rescanning", terr)
		var err error
		trees, err = s.loadGitTrees(defaultBranch)
		if err != nil {
			startupStage("reload: snapshot path failed to build trees (%v); full rebuild", err)
			return false
		}
	}

	// 核对分支集合：比快照多出来的分支要整条补扫，少掉的要摘掉。
	//
	// 这里**不再**因为「head 变了」整份弃用。
	//
	// 踩过的坑：早先的判据是「所有分支的 head 必须与快照一致」。
	// 但这是个活的系统——后台每 30 秒折叠一次脏层、语义索引每 30 秒
	// 追平、分身随时在写。11 条分支全部一步没动过的概率接近零，
	// 于是快照几乎永远被拒，一次都没生效过（实测两次启动都是
	// 「main.dev moved since the snapshot; full rebuild」）。
	//
	// 正确做法是承认它会动，然后把差量补上：`git Diff` 是纯哈希比较，
	// 不读任何 blob；改动的那几个 path 走已有的 refreshPath（O(1)）。
	// 一条分支推进了几个文件，就只重算那几个。
	current := map[string]string{}
	treeByKey := map[string]*memory.Tree{}
	addedBranches := false
	for _, t := range trees {
		repo, rerr := s.projects.Get(t.Project)
		if rerr != nil {
			continue
		}
		if sha, serr := repo.Resolve(t.Branch); serr == nil {
			key := treeHeadKey(t.Project, t.Branch)
			current[key] = sha
			treeByKey[key] = t
			if _, had := hdr.Heads[key]; !had {
				addedBranches = true
			}
		}
	}
	for key := range hdr.Heads {
		if _, ok := current[key]; !ok {
			// 快照里有、现在没有：那条分支被归档/删除了。
			// 树快照里也就不该再有它——restoreTreesFromSnapshot
			// 已经会因此拒绝。走到这里说明树快照没有它，安全。
			continue
		}
	}

	idx, err := index.ReadBinary(index.SnapshotPath(dir))
	if err != nil {
		startupStage("reload: snapshot data unreadable (%v); full rebuild", err)
		return false
	}
	// 文档数不一致 → 治愈，不弃用。
	//
	// 根因（2026-11-19 查出）：saveIndexSnapshot 先调 ix.Docs() 记账，
	// 隔着一个拷贝整棵树的循环之后才调 ix.TakeSnapshot() 取数据。
	// 索引内部无锁，这两步之间只要有并发写入，取到的数据就比账本多。
	// 于是文件里 37,683 篇、头部写着 37,682 篇——**差一篇就整份作废**，
	// 线上实测代价是 40 分钟全量冷建，而真相只是毫秒级的记录偏差。
	//
	// 快照是缓存不是真相源（Git 才是）。二进制既然读通了，结构就是自洽的；
	// 差的那几篇交给下面的差量补丁 + healIndexAgainstTrees 对齐即可。
	drift := idx.Docs() != hdr.Docs
	if drift {
		startupStage("reload: snapshot doc count drifted (file %d vs header %d); healing instead of rebuilding",
			idx.Docs(), hdr.Docs)
	}
	if addedBranches {
		startupStage("reload: snapshot predates one or more branches; healing index for the added trees")
	}

	// 应用差量：把「快照之后每个分支改了什么」补进索引。
	//
	// 这是让快照真正可用的关键一步。不做这一步的话，只要有一个分支
	// 动过，快照就永远被拒——而这是活系统，几乎每次都动。
	//
	// 差量用 repo.Diff：纯哈希比较，不读任何 blob。
	// 改动的那几个 path 走 refreshPath（O(路径深度)）。
	patched := 0
	for key, curSHA := range current {
		prevSHA, had := hdr.Heads[key]
		if !had || prevSHA == curSHA {
			continue
		}
		proj, br, ok := splitTreeHeadKey(key)
		if !ok {
			continue
		}
		repo, rerr := s.projects.Get(proj)
		if rerr != nil {
			continue
		}
		changes, derr := repo.Diff(prevSHA, curSHA)
		if derr != nil {
			// 差量算不出来（历史被重写、对象缺失）——这一条分支整条重建。
			startupStage("reload: cannot diff %s (%v); rescanning that branch",
				splitKeyForLog(key), derr)
			if tree, terr := s.scanGitProject(repo, proj, br); terr == nil {
				for i, t := range trees {
					if t.Project == proj && t.Branch == br {
						trees[i] = tree
						break
					}
				}
			}
			continue
		}
		// 把树里这一条分支的 Blocks 同步成新状态，并更新索引。
		for _, ch := range changes {
			patched++
			applyOneChange(trees, idx, repo, proj, br, ch)
		}
	}

	// 快照里有、现在没有的分支：从索引里摘掉。
	for key := range hdr.Heads {
		if _, ok := current[key]; ok {
			continue
		}
		proj, br, ok := splitTreeHeadKey(key)
		if !ok {
			continue
		}
		for _, t := range trees {
			if t.Project != proj || t.Branch != br {
				continue
			}
			for _, b := range t.Indexable() {
				idx.Remove(b.ID())
			}
		}
	}

	if patched > 0 {
		startupStage("reload: applied %d changed path(s) from diff", patched)
	}

	// 计数漂移或新增分支时，与树逐条对齐。新增分支只有 tree
	// 被补扫，快照的倒排表里没有它的文档，必须走一次补全。
	if drift || addedBranches {
		removed, added, orphans := s.healIndexAgainstTrees(idx, trees)
		startupStage("reload: healed index against trees (removed %d, added %d); %d docs now",
			removed, added, idx.Docs())
		for _, addr := range orphans {
			startupStage("reload:   dropped orphan card: %s", addr)
		}
		if removed > orphanAlarmThreshold {
			// 超过阈值就不再当日常删除看待：一整条分支消失、或者对象丢失，
			// 都长这个样子，而它只会在日志里出现这一次。
			startupStage("reload: WARNING %d orphan card(s) dropped this run — "+
				"too many for routine deletes; verify the Git repo before trusting search results",
				removed)
		}
	}

	// SKILL.md 的策略必须从磁盘重新解析：它决定「谁能看见什么」，
	// 而权限绝不能来自一份可能过期的快照。
	for _, t := range trees {
		for _, b := range t.Blocks {
			if b.Path == "SKILL.md" || strings.HasSuffix(b.Path, "/SKILL.md") {
				if body, rerr := s.readBlock(b); rerr == nil {
					s.policies.Store(t.Project, manifest.Parse(t.Project, body))
				}
			}
		}
	}

	s.seedDedup(trees)

	s.publishTrees(trees, idx)

	startupStage("reload: index from snapshot: %d docs, %d terms in %.1fs",
		idx.Docs(), idx.Terms(), time.Since(t0).Seconds())
	return true
}

func splitKeyForLog(key string) string {
	return strings.ReplaceAll(key, "\x00", "@")
}

// indexSubsetOf 检查索引里的每个 doc 都还在树里。
func indexSubsetOf(idx *index.Index, live map[string]bool) bool {
	for addr := range idx.AllDocs() {
		if !live[addr] {
			return false
		}
	}
	return true
}

// saveIndexSnapshot 把当前索引落盘。
//
// 什么时候存：后台维护周期里存一次，以及退出前存一次。
// 不放在每次写入上——快照是「启动加速」，不是「持久化」，
// 丢一次最多是下次启动慢，不会丢数据（Git 才是真相源）。
func (s *Server) saveIndexSnapshot() error {
	dir := s.indexSnapshotDir()
	if dir == "" || os.Getenv("ZERO_MEMCP_NO_INDEX_SNAPSHOT") != "" {
		return nil
	}
	// 索引内部已无锁（见 internal/index），所以这里不需要任何互斥：
	// TakeSnapshot 遍历的是 sync.Map / 原子发布的分词倒排，
	// 并发写入不会撕裂它，也不会触发 fatal error: concurrent map
	// iteration and map write。
	//
	// 早先这段必须持 s.mu.RLock()，因为 Index 内部是裸 map、
	// 靠那把锁保护。教训记在这里：那把锁漏过两次，第二次是
	// 进程直接没掉，没有任何 recover 机会。
	ix := s.idxNow()
	if ix == nil {
		return nil
	}
	treeNow := s.treeList()
	heads := make(map[string]string, 16)
	trees := make([]string, 0, 16)
	for _, t := range treeNow {
		trees = append(trees, treeHeadKey(t.Project, t.Branch))
	}
	treeSnaps := make([]index.TreeSnapshot, 0, len(treeNow))
	for _, t := range treeNow {
		bs := make([]index.BlockSnapshot, len(t.Blocks))
		for i, b := range t.Blocks {
			bs[i] = index.BlockSnapshot{
				Path: b.Path, Size: b.Size, Time: b.Time,
				Rule: b.Rule, Hidden: b.Hidden, Hash: b.Hash,
				Layer: b.Layer,
			}
		}
		treeSnaps = append(treeSnaps, index.TreeSnapshot{
			Project: t.Project, Branch: t.Branch, Blocks: bs,
		})
	}
	snap := ix.TakeSnapshot()

	// heads 从仓库读，不从内存猜：内存里的 head 是「上次刷新时」的，
	// 而快照必须精确记录「这份索引对应哪个 commit」。
	for _, key := range trees {
		proj, br, ok := splitTreeHeadKey(key)
		if !ok {
			continue
		}
		repo, err := s.projects.Get(proj)
		if err != nil {
			continue
		}
		if sha, err := repo.Resolve(br); err == nil && sha != "" {
			heads[key] = sha
		}
	}

	// 树快照：把每个 Block 的判定结果存下来，下次启动就不必再扫一遍。
	if err := index.SaveTrees(dir, treeSnaps); err != nil {
		return err
	}

	// Docs/Terms 必须取自 snap 自己，不能取自活的 *Index。
	//
	// 踩过的坑：这里原本写的是 docs, terms := ix.Docs(), ix.Terms()，
	// 而它到 TakeSnapshot 之间隔着上面那一大段拷贝。索引无锁，
	// 并发写入会让「账本」和「数据」差一两篇，下次启动就被判为损坏、
	// 整份快照作废、全量冷建 40 分钟。同一份 snap 里的 Addrs/Terms
	// 就是写进文件的那两样东西，用它们的长度记账，头部与数据永远同源。
	hdr := &index.SnapshotHeader{Heads: heads, Docs: len(snap.Addrs), Terms: len(snap.Terms), TreeFile: "index-trees.gob"}
	// 先写倒排表（紧凑格式），再写头部——头部的存在等于「这份快照可用」。
	// 顺序反了的话，进程在两步之间被杀会留下「头部说有、数据却没有」。
	// 锁外写盘：用的是刚才复制出来的 snap，不碰 *Index。
	if err := index.WriteSnapshot(snap, index.SnapshotPath(dir)); err != nil {
		return err
	}
	if err := index.SaveSnapshot(dir, hdr, nil); err != nil {
		return err
	}
	if st, err := os.Stat(index.SnapshotPath(dir)); err == nil {
		s.logf("index snapshot saved: %d docs, %d terms, %.1f MB",
			hdr.Docs, hdr.Terms, float64(st.Size())/(1<<20))
	}
	return nil
}

// restoreTreesFromSnapshot 从树快照重建内存树（不扫盘）。
//
// 每个 Block 的 Read 闭包按 (project, branch, path) 重建——这正是
// scanGitProject 里那个闭包做的事，只是跳过了「读一遍判 frontmatter」。
// Rule / Hidden 直接用快照里的值：它们是判定结果，重算没有意义。
func (s *Server) restoreTreesFromSnapshot(defaultBranch string) ([]*memory.Tree, error) {
	dir := s.indexSnapshotDir()
	if dir == "" {
		return nil, errNoSnapshot
	}
	snaps, err := index.LoadTrees(dir)
	if err != nil {
		return nil, err
	}
	// 项目集合必须与当前配置一致，否则说明配置变了，快照不能信。
	declared := map[string]bool{}
	for _, p := range s.declaredProjects() {
		declared[p] = true
	}
	// 现存的 (项目, 分支) 集合。
	existingBranches := map[string]map[string]bool{}
	for p := range declared {
		repo, rerr := s.projects.Get(p)
		if rerr != nil {
			continue
		}
		set := map[string]bool{}
		for _, br := range repo.Branches() {
			set[br] = true
		}
		// 尚未提交的工作层分支也是真实数据，不能只因为它还没有
		// refs/heads 就漏掉。
		for _, br := range repo.PendingRefs() {
			set[br] = true
		}
		existingBranches[p] = set
	}

	var out []*memory.Tree
	restored := map[string]map[string]bool{}
	for _, ts := range snaps {
		if !declared[ts.Project] {
			return nil, fmt.Errorf("snapshot has project %q which is no longer declared", ts.Project)
		}
		if _, rerr := s.projects.Get(ts.Project); rerr != nil {
			return nil, fmt.Errorf("open project %s: %w", ts.Project, rerr)
		}
		// 分支必须还存在：被归档的分支不能从快照里复活。
		//
		// 判据用 Branches()（与 branch 动词同一份），不用 Resolve：
		// Resolve 会退到 packed-refs，而归档只删 refs/heads 下的文件——
		// 一个 sha 还在 packed-refs 里的分支，Resolve 说「存在」，
		// 但它已经不在 branch 列表里了。两处判据不一致的后果是
		// 「归档了但还能 recall 到」。
		if !existingBranches[ts.Project][ts.Branch] {
			return nil, fmt.Errorf("branch %s@%s no longer exists", ts.Project, ts.Branch)
		}

		t := &memory.Tree{Project: ts.Project, Branch: ts.Branch}
		t.Blocks = make([]memory.Block, len(ts.Blocks))
		for i, bs := range ts.Blocks {
			proj, br, path := ts.Project, ts.Branch, bs.Path
			t.Blocks[i] = memory.Block{
				Project: proj,
				Branch:  br,
				Path:    path,
				Size:    bs.Size,
				Time:    bs.Time,
				Rule:    bs.Rule,
				Hidden:  bs.Hidden,
				Hash:    bs.Hash,
				Layer:   bs.Layer,
				Read: func() ([]byte, error) {
					r, err := s.projects.Get(proj)
					if err != nil {
						return nil, err
					}
					return r.Read(br, path)
				},
			}
		}
		out = append(out, t)
		if restored[ts.Project] == nil {
			restored[ts.Project] = map[string]bool{}
		}
		restored[ts.Project][ts.Branch] = true
	}

	// 快照是「当时有哪些树」，不是「现在只允许有哪些树」。
	// 对快照之后新建的分支（包括全新项目里的分支）逐条补扫；
	// 否则它们既不参与后面的 head diff，也不会被全量扫描，
	// 表现为数据在 Git 里、read 能读，recall/grep 永远看不到。
	for proj, branches := range existingBranches {
		repo, rerr := s.projects.Get(proj)
		if rerr != nil {
			return nil, rerr
		}
		for br := range branches {
			if restored[proj][br] {
				continue
			}
			startupStage("reload: branch %s@%s is absent from snapshot; scanning it", proj, br)
			t, serr := s.scanGitProject(repo, proj, br)
			if serr != nil {
				return nil, fmt.Errorf("scan new branch %s@%s: %w", proj, br, serr)
			}
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, errNoSnapshot
	}
	return out, nil
}

var errNoSnapshot = errors.New("no index snapshot")

// applyOneChange 把一个 git 差量应用到内存树与索引上。
//
// 这是 refreshPath 的「启动期版本」：不取 s.mu（调用方还没发布 trees），
// 直接改传入的树切片与索引。
func applyOneChange(trees []*memory.Tree, idx *index.Index, repo *gitstore.Repo, proj, br string, ch gitstore.Change) {
	var tree *memory.Tree
	for _, t := range trees {
		if t.Project == proj && t.Branch == br {
			tree = t
			break
		}
	}
	if tree == nil {
		return
	}

	id := memory.MakeID(proj, br, ch.Path)

	if ch.Kind == "deleted" {
		idx.Remove(id)
		for i := range tree.Blocks {
			if tree.Blocks[i].Path == ch.Path {
				tree.Blocks = append(tree.Blocks[:i], tree.Blocks[i+1:]...)
				break
			}
		}
		return
	}

	// 新增或修改：取回新条目与内容，重新判定 Rule/Hidden。
	entry, ok, serr := repo.Stat(br, ch.Path)
	if serr != nil || !ok {
		return
	}
	p, b := proj, br
	path := ch.Path
	read := func() ([]byte, error) { return repo.Read(b, path) }
	blk := memory.Block{
		Project: p, Branch: b, Path: path,
		Size: entry.Size, Time: entry.Time, Hash: entry.Hash, Read: read,
	}
	data, rerr := read()
	if rerr != nil {
		return
	}
	_, rule, hidden := classifyContent(p, path, data, entry.Mode)
	blk.Rule = rule
	blk.Hidden = hidden

	idx.Remove(id)
	if !blk.Hidden {
		stamp := entry.Time
		if at := sliceAtTime(string(data), path); at > 0 {
			stamp = at
		}
		idx.AddWithTime(id, string(data), stamp)
	}

	for i := range tree.Blocks {
		if tree.Blocks[i].Path == path {
			tree.Blocks[i] = blk
			return
		}
	}
	tree.Blocks = append(tree.Blocks, blk)
}

// healIndexAgainstTrees 把索引修剪到与当前树列表完全一致。
//
// 什么时候调：只在快照头部计数与数据文件发生漂移时作为自愈步骤。
//
// 两步：
//  1. 删孤儿：快照索引里有、但任何活跃树里都没有的条目（比如快照后被删的未提交文件），
//     从索引剔除。
//  2. 补漏条：树里声明可索引、但快照索引里没有的，读正文补进去。
//
// 纯内存 O(Docs) 遍历，4 万条耗时约 5~10ms，相比 40 分钟冷建是 20 万倍收益。
// 返回被剔除的地址样本（最多 orphanSampleLimit 条）：孤儿卡片是「记录消失」的
// 唯一物证，清掉它必须留下痕迹，不能静默。
func (s *Server) healIndexAgainstTrees(idx *index.Index, trees []*memory.Tree) (removed, added int, orphans []string) {
	if idx == nil {
		return 0, 0, nil
	}
	// 汇总所有活跃树的可索引 Block 集合
	liveBlocks := make(map[string]memory.Block, 4096)
	for _, t := range trees {
		for _, b := range t.Indexable() {
			liveBlocks[b.ID()] = b
		}
	}

	// 1. 剔除不在树里的孤儿 doc
	//
	// 孤儿卡片有两种来源，含义天差地别：
	//   ① 正常删除（delete 动词、清理写错地方的记录）——清掉即可；
	//   ② 异常消失（对象丢失、分支被重置、合并出岔子）——此时这张卡片
	//      是全库唯一还记得「这里曾有过一篇」的物证。
	// 所以清归清，地址要留下来给调用方打日志。
	for addr := range idx.AllDocs() {
		if _, ok := liveBlocks[addr]; !ok {
			idx.Remove(addr)
			removed++
			if len(orphans) < orphanSampleLimit {
				orphans = append(orphans, addr)
			}
		}
	}

	// 2. 补全索引缺失的 block
	idxDocs := idx.AllDocs()
	for id, b := range liveBlocks {
		if _, ok := idxDocs[id]; ok {
			continue
		}
		data, err := s.readBlock(b)
		if err != nil || len(data) == 0 {
			continue
		}
		stamp := b.Time
		if at := sliceAtTime(string(data), b.Path); at > 0 {
			stamp = at
		}
		idx.AddWithTime(id, string(data), stamp)
		added++
	}
	return removed, added, orphans
}

// orphanSampleLimit 是一次自愈里最多留证的孤儿地址条数。
// 只取样不上报全量：正常清理可能是几百条，全打出来只会淹没日志；
// 而真正要看的是「有没有、大概什么形态」，十条足够。
const orphanSampleLimit = 10

// orphanAlarmThreshold 是一次自愈里「多到不像日常删除」的阈值。
const orphanAlarmThreshold = 100
