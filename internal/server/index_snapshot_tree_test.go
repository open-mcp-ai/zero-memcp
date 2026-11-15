package server

import (
	"fmt"
	"sort"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// 树快照必须产出与真扫完全等价的树。
//
// 这是启动加速的正确性底线。树快照跳过了「读每个文件判 frontmatter」，
// 而 Rule / Hidden 正是那次读盘的**结果**。存错了的症状是
// 「规则不注入了」或「该搜的搜不到」，而且完全不报错。
func TestTreeSnapshotMatchesRealScan(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 造出各种形态：普通 / 规则（frontmatter inject）/ 隐藏（index: none）/
	// 子目录 / 非 md 文件 / 规则目录的兄弟文件。
	cases := map[string]string{
		"plain.md":    "普通正文",
		"rule.md":     "---\ninject: every_turn\n---\n\n这是规则",
		"hidden.md":   "---\nindex: none\n---\n\n不进检索",
		"sub/deep.md": "子目录里的",
		"sub/also.md": "另一个",
		"script.sh":   "#!/bin/sh\necho hi\n",
		"fact/f1.md":  "事实一",
	}
	for p, body := range cases {
		if _, err := callMCPVars(t, srv, tok, "blackegg/me@fact:"+p, body); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 真扫一遍，作为参照。
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	realTree, err := srv.scanGitProject(repo, "blackegg/me", "fact")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// 存快照 → 从快照恢复。
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	restored, err := srv.restoreTreesFromSnapshot("zero")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	find := func(trees []*memory.Tree) *memory.Tree {
		for _, tr := range trees {
			if tr.Project == "blackegg/me" && tr.Branch == "fact" {
				return tr
			}
		}
		return nil
	}
	a, b := find([]*memory.Tree{realTree}), find(restored)
	if a == nil || b == nil {
		t.Fatalf("找不到目标分支：real=%v restored=%v", a != nil, b != nil)
	}

	view := func(tr *memory.Tree) []string {
		var out []string
		for _, bl := range tr.Blocks {
			out = append(out, fmt.Sprintf("%s|%d|rule=%v|hidden=%v|hash=%s",
				bl.Path, bl.Size, bl.Rule, bl.Hidden, bl.Hash))
		}
		sort.Strings(out)
		return out
	}
	va, vb := view(a), view(b)
	if len(va) != len(vb) {
		t.Fatalf("块数不同：真扫 %d，快照 %d\n真扫: %v\n快照: %v", len(va), len(vb), va, vb)
	}
	for i := range va {
		if va[i] != vb[i] {
			t.Fatalf("第 %d 个块不同：\n真扫: %s\n快照: %s", i, va[i], vb[i])
		}
	}

	// 内容也必须读得到（Read 闭包要正确重建）。
	for _, bl := range b.Blocks {
		body, rerr := srv.readBlock(bl)
		if rerr != nil {
			t.Fatalf("从快照恢复的块读不出内容（%s）：%v", bl.Path, rerr)
		}
		if len(body) == 0 && bl.Size > 0 {
			t.Fatalf("从快照恢复的块读出来是空的（%s），但大小是 %d", bl.Path, bl.Size)
		}
	}
}

// 分支被归档之后，快照里那条分支不能复活。
func TestTreeSnapshotDoesNotResurrectArchivedBranch(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:x.md -- x"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m one"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 归档那条分支。
	if _, err := callMCP(t, srv, tok, "archive blackegg/me@fact"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if _, err := srv.restoreTreesFromSnapshot("zero"); err == nil {
		t.Fatal("分支已归档，快照仍然被采用——已归档的分支不该从快照里复活")
	}
}
