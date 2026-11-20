package server

import (
	"fmt"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/index"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// treeSet 是一次「记忆库长什么样」的不可变快照。
//
// 约束（发布纪律，改这个文件之前先读）：
//   - trees 切片发布后不再被 append/替换元素；
//   - 每个 *memory.Tree 发布后其 Blocks 不再被就地修改（要改就造新的 Tree）；
//   - idx 内部已经是无锁结构（见 internal/index），所以它自己不需要版本。
//
// 违反其中任何一条，读者就会看到半新半旧的状态——那比崩溃更难查。
type treeSet struct {
	trees []*memory.Tree
	idx   *index.Index
}

// treeWriteTimeout 是等写令牌的上界。
//
// 写路径本质是「重建某个分支的索引 + 换掉树」，可能涉及磁盘 I/O；
// 给它一个明确的上界，超时就报错，而不是让后来的写者无限排队。
const treeWriteTimeout = 60 * time.Second

// loadTrees 取当前快照（可能为 nil）。
func (s *Server) loadTrees() *treeSet { return s.treesPtr.Load() }

// treeList 返回当前树列表（不含索引）。永不返回 nil。
func (s *Server) treeList() []*memory.Tree {
	if ts := s.loadTrees(); ts != nil {
		return ts.trees
	}
	return nil
}

// idxNow 返回当前索引。可能为 nil（未建好）。
func (s *Server) idxNow() *index.Index {
	if ts := s.loadTrees(); ts != nil {
		return ts.idx
	}
	return nil
}

// branchTok 返回某条分支的写令牌（懒创建，每分支一把）。
//
// 为什么按分支分段而不是全局一把：写一条记忆只需要重建它所在的那条
// 分支的索引。全局令牌会让「agent A 写一条」等「agent B 重建一个
// 五千篇的分支」——那几秒里所有 agent 都写不进去。
// 分段后：不同分支完全并行，同分支排队（这是正确的，它们要改同一棵树）。
//
// 必须懒创建：Server 有零值构造路径（测试、早期装配），
// nil channel 上的 select 永久阻塞——症状是写入卡满 60 秒再超时。
func (s *Server) branchTok(project, branch string) chan struct{} {
	key := project + "@" + branch
	if v, ok := s.branchToks.Load(key); ok {
		return v.(chan struct{})
	}
	tok := make(chan struct{}, 1)
	v, _ := s.branchToks.LoadOrStore(key, tok)
	return v.(chan struct{})
}

// acquireBranchWrite 取某条分支的写令牌。返回的 release 必须调用。
func (s *Server) acquireBranchWrite(project, branch string) (func(), error) {
	tok := s.branchTok(project, branch)
	select {
	case tok <- struct{}{}:
		return func() { <-tok }, nil
	default:
	}
	t := time.NewTimer(treeWriteTimeout)
	defer t.Stop()
	select {
	case tok <- struct{}{}:
		return func() { <-tok }, nil
	case <-t.C:
		return nil, fmt.Errorf("%s@%s: 分支写令牌等待超时（%v）：有写者卡在重建这条分支的索引上",
			project, branch, treeWriteTimeout)
	}
}

// publishTrees 用新的树列表替换当前快照（必须在持写令牌时调用）。
func (s *Server) publishTrees(trees []*memory.Tree, idx *index.Index) {
	s.treesPtr.Store(&treeSet{trees: trees, idx: idx})
}

// cloneTree 复制一棵树。写路径必须先 clone 再改，绝不能动已发布的树。
func cloneTree(t *memory.Tree) *memory.Tree {
	cp := &memory.Tree{Project: t.Project, Branch: t.Branch}
	cp.Blocks = make([]memory.Block, len(t.Blocks))
	copy(cp.Blocks, t.Blocks)
	return cp
}

// cloneTreeList 复制树列表（元素仍指向原树，需要改哪棵再单独 cloneTree）。
func cloneTreeList(trees []*memory.Tree) []*memory.Tree {
	out := make([]*memory.Tree, len(trees))
	copy(out, trees)
	return out
}

// findTree 在当前树列表里按 (project, branch) 找树的下标。找不到返回 -1。
func findTree(trees []*memory.Tree, project, branch string) int {
	for i, t := range trees {
		if t.Project == project && t.Branch == branch {
			return i
		}
	}
	return -1
}

// testGlobalWriteTok 是测试专用的全库写令牌。
func (s *Server) testGlobalWriteTok() func() {
	tok := s.branchTok("_test", "_global")
	tok <- struct{}{}
	return func() { <-tok }
}
