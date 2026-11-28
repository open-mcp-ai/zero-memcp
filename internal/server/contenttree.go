package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// contentTreeKey 是内容树缓存的键。
//
// 键取的是**输入**，不是「(project, branch) 这个位置」。
//
// 内容树是这两个输入的纯函数——FlattenBranch 的定义就是「已提交的树
// 叠加脏层」——所以同一个键必然产出同一棵树，缓存**不可能过期**。
//
// 为什么不做「写入时让缓存失效」：那条路要求每一个能改动内容的入口
// （noteWriteOp / commitOpsLocal / applyTreeReplace / erase / surgery /
// merge / apply / 补正文 …）都记得失效一次。漏一处，同步就会拿着一棵
// 过期的内容树去跟对端比对：轻则每轮重发整树快照、把「同步很慢」留成
// 一个查不出来的现象，重则把真实差异当成一致。
//
// 失效逻辑会漏，键不会。（2026-11-28）
type contentTreeKey struct {
	project    string
	branch     string
	commitTree string
	// dirty 是脏层指纹：脏层为空时是空串。
	// 不能只记「脏层有几条」——把一个已有路径改成新内容，条数不变而树变了。
	dirty string
}

// contentTreeCacheLimit 是缓存条数上界。
//
// 为什么要上界：脏层指纹每次写入都会变，键是持续增长的。
// 满了就整体清空——条目小、重建一次是毫秒级，不值得为它写 LRU。
const contentTreeCacheLimit = 512

// contentTreeCache 缓存内容树。
//
// 用 sync.Map：每个键互相独立，没有跨键不变量，不是锁
// （见 internal/guard 对可放行原语的说明）。
type contentTreeCache struct {
	entries sync.Map // contentTreeKey -> string
	n       atomic.Int64
	hits    atomic.Int64
	misses  atomic.Int64
}

func (c *contentTreeCache) get(k contentTreeKey) (string, bool) {
	if v, ok := c.entries.Load(k); ok {
		c.hits.Add(1)
		return v.(string), true
	}
	c.misses.Add(1)
	return "", false
}

func (c *contentTreeCache) put(k contentTreeKey, tree string) {
	// 空结果不入缓存：空通常来自一次读失败或空分支，
	// 缓存它会把「读不出来」固化成「没有内容树」。
	if tree == "" {
		return
	}
	if c.n.Load() >= contentTreeCacheLimit {
		c.entries.Range(func(key, _ any) bool {
			c.entries.Delete(key)
			return true
		})
		c.n.Store(0)
	}
	c.entries.Store(k, tree)
	c.n.Add(1)
}

// contentTreeKeyFor 由当前状态算出缓存键。
//
// 第二个返回值是假表示「脏层读不出来」——这时**不能**猜一个键，
// 否则会把一棵来路不明的内容树缓存成某个合法状态。
func contentTreeKeyFor(repo *gitstore.Repo, project, branch string) (contentTreeKey, bool) {
	k := contentTreeKey{project: project, branch: branch}

	// 已提交树：O(1)。注意这一层只是键的一部分，不是内容树本身——
	// 已提交树里可能带着空目录，而内容树按定义只含文件
	// （WriteTreeFromPaths 不为空目录写 tree），两者并不相等。
	// 这次实测确认过：main 分支上提交树 643eacf2 与内容树 817f5509
	// 内容相同而 SHA 不同，差的就是一个空目录。
	if sha, err := repo.Resolve(branch); err == nil && sha != "" {
		if t, terr := repo.TreeOfCommit(sha); terr == nil {
			k.commitTree = t
		}
	}

	changes, err := repo.DirtyChanges(branch)
	if err != nil {
		return k, false
	}
	if len(changes) > 0 {
		// DirtyChanges 已按路径排序（gitstore/dirty.go），指纹因此稳定。
		h := sha256.New()
		for _, ch := range changes {
			fmt.Fprintf(h, "%s\x00%t\x00%s\x00%s\n", ch.Path, ch.Deleted, ch.Mode, ch.SHA)
		}
		k.dirty = hex.EncodeToString(h.Sum(nil))
	}
	return k, true
}
