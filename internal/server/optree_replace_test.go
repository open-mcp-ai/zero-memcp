package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 整树同步只加只改：缺一个路径不是删除的证据。
//
// 演进（2026-11-26）：早先这里钉的是「本地有、权威整树里没有 → 墓碑」，
// 依据是「整树同步不能只加不减，否则删掉的东西会复活」。那条规则把
// 「对端手上没有」当成了「对端删过」，而最常见的成因恰恰是「本地刚写完
// 还没推出去」。生产环境因此累计丢了 347 个文件的内容：笔记本写完 41 个
// 切片并折叠成 commit（推送前的正常动作），紧接着先拉一轮，3060 的树里
// 还没有它们，于是回了一串删除命令把它们全删了。
//
// 现在的语义：
//  1. 删除是**显式动作**——一条 opDelete 命令，沿命令流到达对端。
//     整树快照只负责「把对端有的搬过来」，不负责推断谁该消失。
//  2. 本地有、快照里没有 → 保留，并进 Kept 点名（既不是静默忽略，
//     也不冒充「两端已经一致」）。
//  3. 显式 delete 命令照旧生效（见 TestOpsSyncDeleteIsACommand）。
func TestTreeReplaceKeepsPathsMissingFromAuthoritativeTree(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	write := func(path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, srv, tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  fmt.Sprintf("write daily@zero:%s $BODY", path),
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// 本地已有内容（模拟一台落后节点的现状）：
	//   keep.md / stale.md 已提交（陈旧节点的一部分）
	//   local-edit.md 是本地未提交的新改动，不能被覆盖
	write("keep.md", "留下的")
	write("stale.md", "上游已删掉的")
	if _, err := callMCP(t, srv, tok, "commit daily@zero -m stale-base"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	write("local-edit.md", "本地正在改的")

	// 构造一条权威整树：只含 keep.md 与 local-edit.md 的新版本，
	// stale.md 不在里面 —— 上游已经删了它。
	repo, err := srv.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	keepEntry, ok, err := repo.ReadPathSHA("zero", "keep.md")
	if err != nil || !ok {
		t.Fatalf("读 keep.md: %v %v", err, ok)
	}
	editEntry, ok, err := repo.ReadPathSHA("zero", "local-edit.md")
	if err != nil || !ok {
		t.Fatalf("读 local-edit.md: %v %v", err, ok)
	}

	op := treeOp{
		Seq: 1, Node: "upstream", Kind: opTree, Replace: true,
		Batch: []opEntry{
			{Path: "keep.md", Mode: string(keepEntry.Mode), SHA: keepEntry.SHA},
			{Path: "local-edit.md", Mode: string(editEntry.Mode), SHA: editEntry.SHA},
		},
	}

	res, err := srv.applyOps("daily", "zero", "upstream", []treeOp{op})
	if err != nil {
		t.Fatalf("applyTreeReplace: %v", err)
	}

	// 1. 不在批次里的 stale.md 必须**保留**：缺一个路径不是删除的证据。
	if hasConflictOrDeleted(res, "stale.md") || deletedLocally(t, repo, "stale.md") {
		t.Fatalf("权威整树里缺失的路径不得被当成删除，实际 res=%+v", res)
	}
	if _, err := callMCP(t, srv, tok, "read daily@zero:stale.md"); err != nil {
		t.Fatalf("stale.md 应当保留（对端没有≠对端删过）: %v", err)
	}

	// 2. 保留必须点名，不能静默：否则调用方会把它当成「两端已一致」。
	if !containsStr(res.Kept, "stale.md") {
		t.Fatalf("保留的路径必须出现在 Kept 里，实际 res=%+v", res)
	}

	// 3. 批次里的路径仍在。
	if _, err := callMCP(t, srv, tok, "read daily@zero:keep.md"); err != nil {
		t.Fatalf("keep.md 应保留: %v", err)
	}
}

// 本地未提交改动不能被一次远端整树静默覆盖。
func TestTreeReplaceKeepsLocallyModifiedPaths(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	write := func(path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, srv, tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  fmt.Sprintf("write daily@zero:%s $BODY", path),
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// 本地已提交一条，然后又有未提交改动。
	write("shared.md", "已提交的版本")
	if _, err := callMCP(t, srv, tok, "commit daily@zero -m base"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	write("shared.md", "本地未提交的新版本")

	repo, _ := srv.projects.Get("daily")
	cur, _, _ := repo.ReadPathSHA("zero", "shared.md")

	// 上游的权威整树里没有这个路径（上游认为它被删了）。
	op := treeOp{Seq: 1, Node: "upstream", Kind: opTree, Replace: true, Batch: nil}
	res, err := srv.applyOps("daily", "zero", "upstream", []treeOp{op})
	if err != nil {
		t.Fatalf("applyTreeReplace: %v", err)
	}

	// 本地未提交的内容必须还在，并且被点名。
	out, err := callMCP(t, srv, tok, "read daily@zero:shared.md")
	if err != nil {
		t.Fatalf("本地未提交内容被整树吃掉了: %v（res=%+v）", err, res)
	}
	if !strings.Contains(out, "本地未提交") {
		t.Fatalf("读到的是被覆盖的版本:\n%s", out)
	}
	if len(res.Kept) == 0 {
		t.Fatalf("被保留的本地改动必须出现在 Kept 里，实际 res=%+v", res)
	}
	_ = cur
}

// hasConflictOrDeleted 判断某路径是否被登记为删除或冲突。
func hasConflictOrDeleted(res opApplyResult, path string) bool {
	for _, c := range res.Conflicted {
		if c.Path == path {
			return true
		}
	}
	return false
}

// deletedLocally 判断某路径在本地是否已被标记删除。
func deletedLocally(t *testing.T, repo interface {
	Read(string, string) ([]byte, error)
}, path string) bool {
	t.Helper()
	_, err := repo.Read("zero", path)
	return err != nil
}
