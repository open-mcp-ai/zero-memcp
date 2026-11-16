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
	release, err := srv.acquireTreeWrite()
	if err != nil {
		t.Fatalf("acquireTreeWrite: %v", err)
	}
	defer release()

	cur := srv.treeList()
	next := make([]*memory.Tree, len(cur))
	for i, tr := range cur {
		next[i] = cloneTree(tr)
	}
	fn(next)
	srv.publishTrees(next, srv.idxNow())
}
