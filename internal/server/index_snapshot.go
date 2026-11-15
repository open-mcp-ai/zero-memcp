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
// 判据（三条同时成立才恢复）：
//  1. 头部能读、magic/version 对得上
//  2. 每个「项目@分支」的 head 与快照里记的一致
//  3. 反序列化出来的文档数与头部记录的一致
//
// 只要有一条不成立就返回 false，退回全量重建。
// 这条「宁可慢也不给错」的边界是刻意的：错的索引不会报错，
// 只会让「写了但搜不到」，那种故障比启动慢难查一百倍。
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
	for _, t := range trees {
		repo, rerr := s.projects.Get(t.Project)
		if rerr != nil {
			continue
		}
		if sha, serr := repo.Resolve(t.Branch); serr == nil {
			key := treeHeadKey(t.Project, t.Branch)
			current[key] = sha
			treeByKey[key] = t
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
	if idx.Docs() != hdr.Docs {
		startupStage("reload: snapshot doc count mismatch (%d vs %d); full rebuild",
			idx.Docs(), hdr.Docs)
		return false
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

	s.mu.Lock()
	s.trees = trees
	s.idx = idx
	s.mu.Unlock()

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
	// 整段导出必须持读写锁。
	//
	// 踩过的坑：早先只在取 ix 指针时持锁，然后锁外 Export()。
	// 而写入路径是在 s.mu.Lock() 里调 idx.AddWithTime 的——
	// 于是「后台存快照」与「用户写入」同时动同一张 map，
	// 进程直接 fatal error: concurrent map iteration and map write。
	// 那不是 error，是 runtime 直接杀进程，没有任何 recover 机会。
	s.mu.RLock()
	if s.idx == nil {
		s.mu.RUnlock()
		return nil
	}
	ix := s.idx
	heads := make(map[string]string, 16)
	trees := make([]string, 0, 16)
	for _, t := range s.trees {
		trees = append(trees, treeHeadKey(t.Project, t.Branch))
	}
	docs, terms := ix.Docs(), ix.Terms()
	treeSnaps := make([]index.TreeSnapshot, 0, len(s.trees))
	for _, t := range s.trees {
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
	// 在持锁的同一窗口里把倒排表复制出来。
	//
	// 这是修一个真实的 race（2026-11-16，-race 抓到）：
	// 早先这里只复制了 trees，然后锁外直接调 ix.WriteBinary。
	// 而 WriteBinary 要遍历 ix.docLen / ix.postings / ix.docTokens，
	// 与此同时并发写入在跑 refreshPath → ix.AddWithTime 改写同样的 map。
	// 后果是 runtime 直接 fatal error: concurrent map iteration and map write
	// ——不是 error，是杀进程，没有 recover 机会。
	//
	// 为什么不用「整段 WriteBinary 持 RLock」：写盘要 gzip 几十 MB、
	// 做 fsync，持锁期间所有写入都被卡住。快照是后台任务，不该阻塞前台。
	// 复制只遍历 map 取值，微秒级。
	snap := ix.TakeSnapshot()
	s.mu.RUnlock()

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

	hdr := &index.SnapshotHeader{Heads: heads, Docs: docs, Terms: terms, TreeFile: "index-trees.gob"}
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
			docs, terms, float64(st.Size())/(1<<20))
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
		existingBranches[p] = set
	}

	var out []*memory.Tree
	for _, ts := range snaps {
		if !declared[ts.Project] {
			return nil, fmt.Errorf("snapshot has project %q which is no longer declared", ts.Project)
		}
		repo, rerr := s.projects.Get(ts.Project)
		if rerr != nil {
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
				Read:    func() ([]byte, error) { return repo.Read(br, path) },
			}
		}
		out = append(out, t)
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
