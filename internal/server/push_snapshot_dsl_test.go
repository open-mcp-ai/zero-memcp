package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// push --snapshot 端到端验证：
//
// 本地有 3 个提交的历史，通过 DSL push --snapshot --force 推到本地裸仓库，
// 远端只拿到 1 个孤儿提交（历史归零），且内容与本地一致。
func TestPushSnapshotViaDSL(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	write := func(path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, srv, tok, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  fmt.Sprintf("write blackegg/me@zero:%s $BODY", path),
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// 制造 3 个提交的历史
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	write("a.md", "A")
	if _, err := srv.commitBranch("blackegg/me", repo, "zero", "commit 1"); err != nil {
		t.Fatal(err)
	}
	write("b.md", "B")
	if _, err := srv.commitBranch("blackegg/me", repo, "zero", "commit 2"); err != nil {
		t.Fatal(err)
	}
	write("c.md", "C")
	if _, err := srv.commitBranch("blackegg/me", repo, "zero", "commit 3"); err != nil {
		t.Fatal(err)
	}

	headBefore, err := repo.Resolve("zero")
	if err != nil || headBefore == "" {
		t.Fatal("head missing")
	}

	// 准备一个空的目标裸仓库
	destDir := t.TempDir() + "/dest.git"
	if _, err := gitstore.Init(destDir); err != nil {
		t.Fatalf("init dest: %v", err)
	}

	// 注入一个本地路径远端（remotes 是 atomic.Pointer，直接替换）
	rems := []config.Remote{{
		Project: "blackegg/me",
		Name:    "dest",
		URL:     destDir,
		Push:    []string{"zero"},
	}}
	srv.remotes.Store(&rems)

	// 调用 DSL: push blackegg/me dest zero --snapshot
	out, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd": "push blackegg/me dest zero --snapshot",
		},
	})
	if err != nil {
		t.Fatalf("push --snapshot: %v", err)
	}
	outText := fmt.Sprint(out)
	if !strings.Contains(outText, "SNAPSHOT") {
		t.Fatalf("输出应包含 SNAPSHOT 提示，实际: %s", outText)
	}

	// 检查远端：历史长度必须恰好是 1（孤儿提交）
	dest, err := gitstore.Open(destDir)
	if err != nil {
		t.Fatal(err)
	}
	destHead, err := dest.Resolve("zero")
	if err != nil || destHead == "" {
		t.Fatal("远端没有写入分支头")
	}
	if destHead == headBefore {
		t.Fatalf("远端不该是本地有历史的提交 %s，而应是一个孤儿快照提交", headBefore[:8])
	}

	destLog, err := dest.Log("zero", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(destLog) != 1 {
		t.Fatalf("浅拷贝后远端历史必须恰好只有 1 个提交，实际 %d 个", len(destLog))
	}

	// 检查远端内容：a.md, b.md, c.md 都在（内容一个不少）
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		body, rerr := dest.Read("zero", name)
		if rerr != nil {
			t.Fatalf("远端必须能读到 %s: %v", name, rerr)
		}
		if len(body) == 0 {
			t.Fatalf("远端 %s 内容为空", name)
		}
	}

	// 检查本地：历史完全不受影响，HEAD 还是 headBefore
	localHead, _ := repo.Resolve("zero")
	if localHead != headBefore {
		t.Fatalf("本地分支头不该被改动")
	}
	localLog, _ := repo.Log("zero", 0)
	if len(localLog) < 3 {
		t.Fatalf("本地历史不该被缩减，实际 %d 个", len(localLog))
	}
}
