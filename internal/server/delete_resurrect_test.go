package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 显式删除的路径不许被对端的陈旧内容复活。
//
// 「防复活不变量」在 opDelete 那侧早就写着（墓碑仍然落地），但反向那一半
// 漏了：opSet 的冲突判定排除了 Deleted 的本地改动（`!lch.Deleted`），
// 于是对端一份还没收到删除的旧内容，把本地墓碑直接覆盖掉。
//
// 实测形状（2026-11-28 自狗粮）：A 删掉一条内容，B 还没拉到删除就把旧快照
// 推回 A，A 应用后文件原地复活，日志 applied 1、0 conflicted，一声不响。
// 用户看到的是「我删了它又回来了」。
func TestPeerSetCannotResurrectLocallyDeletedPath(t *testing.T) {
	s, _ := newTestServer(t, `
[server]
data = "."

[[project]]
name = "notes"
owner = "alice"

[[user]]
name = "alice"
root = true
`)
	repo, err := s.projects.Ensure("notes")
	if err != nil {
		t.Fatal(err)
	}
	const branch = "zero"

	// 先有一条内容，并让对端也拿到同样的 blob（模拟两边都有这份内容）。
	body := []byte("本端显式删掉的内容")
	if err := repo.Write(branch, "doomed.md", body, gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	sha, err := repo.BlobHash(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PutBlob(sha, body); err != nil {
		t.Fatal(err)
	}

	// 本端显式删除：脏层里留下墓碑。
	if err := repo.Delete(branch, "doomed.md"); err != nil {
		t.Fatal(err)
	}

	// 对端的 set 到达（它还没收到我们的删除）。
	s.applyIncomingOpsForTest(t, branch, "peer", []treeOp{
		{Kind: opSet, Path: "doomed.md", Mode: string(gitstore.ModeContent), SHA: sha},
	})

	if _, ok, _ := repo.ReadPathSHA(branch, "doomed.md"); ok {
		t.Fatal("对端的陈旧内容把本地显式删除复活了——「防复活不变量」被破")
	}
}

// 对端重新写一份**不同内容**时，墓碑仍然优先，但必须留下证据。
func TestResurrectionAttemptIsRecorded(t *testing.T) {
	s, _ := newTestServer(t, `
[server]
data = "."

[[project]]
name = "notes"
owner = "alice"

[[user]]
name = "alice"
root = true
`)
	repo, err := s.projects.Ensure("notes")
	if err != nil {
		t.Fatal(err)
	}
	const branch = "zero"
	if err := repo.Write(branch, "doomed.md", []byte("旧"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	newBody := []byte("对端又写了一份新的")
	newSHA, err := repo.BlobHash(newBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PutBlob(newSHA, newBody); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(branch, "doomed.md"); err != nil {
		t.Fatal(err)
	}

	s.applyIncomingOpsForTest(t, branch, "peer", []treeOp{
		{Kind: opSet, Path: "doomed.md", Mode: string(gitstore.ModeContent), SHA: newSHA},
	})
	if _, ok, _ := repo.ReadPathSHA(branch, "doomed.md"); ok {
		t.Fatal("墓碑被覆盖")
	}

	// 证据必须留下：不然「有东西没落地」无处可查。
	found := false
	s.workSync.conflicts.Range(func(k, v any) bool {
		if c, ok := v.(workConflict); ok && c.Path == "doomed.md" && c.Kind == ConflictDelete {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("复活尝试没有留下证据（冲突表里查不到）")
	}
}

// applyIncomingOpsForTest 把一批命令直接喂给落地层，绕开网络。
func (s *Server) applyIncomingOpsForTest(t *testing.T, branch, peer string, ops []treeOp) {
	t.Helper()
	ctx, err := s.newOpContext("notes", branch)
	if err != nil {
		t.Fatal(err)
	}
	var res opApplyResult
	for _, op := range ops {
		if _, aerr := s.applyOp("notes", branch, peer, ctx, op, &res); aerr != nil {
			if strings.Contains(aerr.Error(), "does not exist") {
				continue
			}
			t.Fatalf("应用命令失败: %v", aerr)
		}
	}
}
