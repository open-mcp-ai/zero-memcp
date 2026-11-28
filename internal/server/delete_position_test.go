package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 对端回报的位点已经越过 opDelete 时，删除仍然必须发出去。
//
// 自狗粮抓到的真 bug（2026-11-28）：差分分支把 batch.To 设成 l.Last()，
// 对端于是回报 after = l.Last()。下一轮导出按位点过滤（l.Since(after)），
// 那条 opDelete 就永远筛不出来——实测日志是每轮「sending 0 explicit
// delete(s)」，对端永远留着本端已经删掉的内容，还把旧快照每 3 秒推回来。
//
// 这条测试故意把 after 传到「越过全部命令」的位置，钉住「位点不能用来
// 判断删除是否已被送达」。
func TestExplicitDeleteIsExportedEvenWhenPeerPositionIsAhead(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	const project, branch = "blackegg/me", "zero"

	repo, err := srv.projects.Ensure(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write(branch, "keep.md", []byte("留着的"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write(branch, "gone.md", []byte("要被删掉的"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit "+project+"@"+branch+" -m seed"); err != nil {
		t.Fatal(err)
	}
	// 对端手上是「删除之前」那棵树（两条都在）。必须先取，删除之后再取
	// 就变成删除后的树，导出会走「对端已经和本端一样」那条提前返回，
	// 测试就测不到东西了。
	peerTree, perr := srv.currentContentTree(project, branch)
	if perr != nil || peerTree == "" {
		t.Fatalf("取删除前的内容树失败: %v", perr)
	}

	// 走真实路径：delete 动词，它会记一条 opDelete。
	if _, err := callMCP(t, srv, tok, "delete "+project+"@"+branch+"/gone.md"); err != nil {
		t.Fatal(err)
	}

	l, err := srv.opLogs.For(project, branch)
	if err != nil {
		t.Fatal(err)
	}
	last := l.Last()
	if last == 0 {
		t.Fatal("delete 之后命令流里应该有内容")
	}

	mine, _ := srv.currentContentTree(project, branch)
	if mine == peerTree {
		t.Fatal("删除之后内容树必须变化，否则这个测试测不到东西")
	}
	if mine == "" || mine == gitstore.EmptyTreeSHA {
		t.Fatalf("本端内容树不该是空（keep.md 还在）: %q", mine)
	}

	// 位点故意给到 l.Last()：模拟「对端回报的位点已经越过那条 opDelete」。
	batch, err := srv.exportOpsFromPeer(project, branch, last, peerTree, peerTree, "peer")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	found := false
	for _, op := range batch.Ops {
		if op.Kind == opDelete && op.Path == "gone.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("显式删除没被导出（位点=%d, 命令流 last=%d, 本批 %d 条命令）——"+
			"对端会永远留着本端已删掉的内容", last, last, len(batch.Ops))
	}
}
