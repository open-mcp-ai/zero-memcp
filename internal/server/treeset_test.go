package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// testTrees 返回当前树列表（测试专用，替代原来直接读 testTrees(srv)）。
func testTrees(srv *Server) []*memory.Tree { return srv.treeList() }

// mutateTrees 取写令牌、克隆树列表、交给 fn 修改、再发布。
//
// 测试里改块（例如替换 b.Read 计数）必须走这条路：已发布的树不可修改。
func mutateTrees(t *testing.T, srv *Server, fn func(trees []*memory.Tree)) {
	t.Helper()
	// 测试要改整份树列表，所以把所有分支的令牌都拿一遍容易死锁；
	// 这里用一个专用的「全库」令牌：测试是单线程改完再发布，
	// 不需要与生产写者真并发。
	release := srv.testGlobalWriteTok()
	defer release()

	cur := srv.treeList()
	next := make([]*memory.Tree, len(cur))
	for i, tr := range cur {
		next[i] = cloneTree(tr)
	}
	fn(next)
	srv.publishTrees(next, srv.idxNow())
}
