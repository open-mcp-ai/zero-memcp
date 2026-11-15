package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	// 核对 head：变了的分支不能信快照里的那些 doc。
	current := map[string]string{}
	for _, t := range trees {
		repo, rerr := s.projects.Get(t.Project)
		if rerr != nil {
			continue
		}
		if sha, serr := repo.Resolve(t.Branch); serr == nil {
			current[treeHeadKey(t.Project, t.Branch)] = sha
		}
	}
	if len(current) != len(hdr.Heads) {
		startupStage("reload: snapshot has %d branch(es), current has %d; full rebuild",
			len(hdr.Heads), len(current))
		return false
	}
	for k, sha := range current {
		if hdr.Heads[k] != sha {
			startupStage("reload: %s moved since the snapshot; full rebuild", splitKeyForLog(k))
			return false
		}
	}

	data, err := index.LoadSnapshotData(dir)
	if err != nil {
		startupStage("reload: snapshot data unreadable (%v); full rebuild", err)
		return false
	}
	idx := index.Import(data)
	if idx.Docs() != hdr.Docs {
		startupStage("reload: snapshot doc count mismatch (%d vs %d); full rebuild",
			idx.Docs(), hdr.Docs)
		return false
	}

	// 只保留仍然存在的 doc：快照与树之间如果出现「索引里有、树里没有」
	// 的条目，说明两边对不上，与其猜不如整份弃用。
	live := make(map[string]bool, 2048)
	for _, t := range trees {
		for _, b := range t.Indexable() {
			live[b.ID()] = true
		}
	}
	if !indexSubsetOf(idx, live) {
		startupStage("reload: snapshot references docs that no longer exist; full rebuild")
		return false
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
	s.mu.RLock()
	ix := s.idx
	heads := make(map[string]string, 16)
	trees := make([]string, 0, 16)
	for _, t := range s.trees {
		trees = append(trees, treeHeadKey(t.Project, t.Branch))
	}
	s.mu.RUnlock()
	if ix == nil {
		return nil
	}

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
	s.mu.RLock()
	treeSnaps := make([]index.TreeSnapshot, 0, len(s.trees))
	for _, t := range s.trees {
		bs := make([]index.BlockSnapshot, len(t.Blocks))
		for i, b := range t.Blocks {
			bs[i] = index.BlockSnapshot{
				Path: b.Path, Size: b.Size, Time: b.Time,
				Rule: b.Rule, Hidden: b.Hidden, Hash: b.Hash,
			}
		}
		treeSnaps = append(treeSnaps, index.TreeSnapshot{
			Project: t.Project, Branch: t.Branch, Blocks: bs,
		})
	}
	s.mu.RUnlock()
	if err := index.SaveTrees(dir, treeSnaps); err != nil {
		return err
	}

	hdr := &index.SnapshotHeader{Heads: heads, Docs: ix.Docs(), Terms: ix.Terms(), TreeFile: "index-trees.gob"}
	if err := index.SaveSnapshot(dir, hdr, ix.Export()); err != nil {
		return err
	}
	if st, err := os.Stat(filepath.Join(dir, "index-data.gob")); err == nil {
		s.logf("index snapshot saved: %d docs, %d terms, %.1f MB",
			ix.Docs(), ix.Terms(), float64(st.Size())/(1<<20))
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
