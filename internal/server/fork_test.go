package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// fork 是零字节分叉：只写一个 ref，不复制对象。
// 三条语义必须成立：源分支不受影响、分叉点完全相同、之后各写各的。
func TestBranchForkIsolation(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/base.md -- 源分支的底稿"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m base"); err != nil {
		t.Fatal(err)
	}
	srcHead, err := repo.Resolve("zero")
	if err != nil || srcHead == "" {
		t.Fatalf("源分支应已提交: %v", err)
	}

	// 显式 from 的分叉
	if _, err := callMCP(t, srv, tok, "branch blackegg/me exp from zero"); err != nil {
		t.Fatalf("fork 失败: %v", err)
	}
	forkHead, err := repo.Resolve("exp")
	if err != nil || forkHead != srcHead {
		t.Fatalf("分叉点必须与源完全相同: %s vs %s (%v)", forkHead, srcHead, err)
	}

	// 分叉后写新分支，源分支必须一字不动
	if _, err := callMCP(t, srv, tok, "write blackegg/me@exp/only-exp.md -- 只属于 exp"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@exp -m exp"); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Resolve("zero"); got != srcHead {
		t.Fatalf("分叉后源分支不该被推进: %s → %s", srcHead, got)
	}
	if _, err := repo.Read("zero", "only-exp.md"); err == nil {
		t.Fatal("exp 的写入泄漏进了 zero")
	}
	if body, err := repo.Read("exp", "only-exp.md"); err != nil || !strings.Contains(string(body), "只属于 exp") {
		t.Fatalf("exp 自己的写入读不到: %v %q", err, string(body))
	}
	// 源分支的内容在分叉里仍然可见（共享对象）
	if body, err := repo.Read("exp", "base.md"); err != nil || !strings.Contains(string(body), "源分支的底稿") {
		t.Fatalf("分叉应继承源内容: %v %q", err, string(body))
	}
}

// 默认分叉源是 persona 分支（不写 from 时）。
func TestBranchForkDefaultsToPersona(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/a.md -- x"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m a"); err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Resolve("zero")

	if _, err := callMCP(t, srv, tok, "branch blackegg/me implicit"); err != nil {
		t.Fatalf("默认分叉失败: %v", err)
	}
	if got, _ := repo.Resolve("implicit"); got != head {
		t.Fatalf("默认应从 persona 分支分叉: %s vs %s", got, head)
	}
}

// 分叉一个不存在的源必须报错，不能造出一个空分支。
func TestBranchForkFromMissingSource(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "branch blackegg/me nope from nosuchbranch"); err == nil {
		t.Fatal("从不存在的分支分叉应报错")
	}
}

// archive：把分支复制成新 ref，源分支移进不可见命名空间，内容一字不丢。
func TestArchiveKeepsContentAndHidesSource(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/keep.md -- 归档也要留住的正文"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m keep"); err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Resolve("zero")

	out, err := callMCP(t, srv, tok, "archive blackegg/me@zero")
	if err != nil {
		t.Fatalf("archive 失败: %v", err)
	}
	_ = out

	// 归档分支里内容仍在
	body, err := repo.Read("zero-archived", "keep.md")
	if err != nil || !strings.Contains(string(body), "归档也要留住的正文") {
		t.Fatalf("归档分支内容丢失: %v %q", err, string(body))
	}
	if got, _ := repo.Resolve("zero-archived"); got != head {
		t.Fatalf("归档分支应指向原提交: %s vs %s", got, head)
	}

	// 源分支不再出现在 branch 列表里（移进隐藏命名空间）
	list, err := callMCP(t, srv, tok, "branch blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "* zero ") || strings.Contains(list, "  zero ") {
		t.Fatalf("归档后源分支不该再出现在列表: %s", list)
	}
	if !strings.Contains(list, "zero-archived") {
		t.Fatalf("归档分支应出现在列表: %s", list)
	}
}

// reset：把目标分支重置到源，且旧版本自动归档——重置不是单向门。
func TestResetArchivesOldVersion(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	// zero 上先有内容 A
	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/a.md -- 内容 A"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m A"); err != nil {
		t.Fatal(err)
	}
	oldHead, _ := repo.Resolve("zero")

	// 分叉出 src，在上面写 B
	if _, err := callMCP(t, srv, tok, "branch blackegg/me src from zero"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "write blackegg/me@src/b.md -- 内容 B"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@src -m B"); err != nil {
		t.Fatal(err)
	}
	srcHead, _ := repo.Resolve("src")

	// zero 再往前走一步，确保 reset 真的把 head 拨回去
	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/c.md -- 内容 C"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m C"); err != nil {
		t.Fatal(err)
	}
	preResetHead, _ := repo.Resolve("zero")
	if preResetHead == oldHead {
		t.Fatal("测试前提不成立：zero 应该已经前进")
	}

	out, err := callMCP(t, srv, tok, "reset blackegg/me@zero from blackegg/me@src")
	if err != nil {
		t.Fatalf("reset 失败: %v", err)
	}
	if !strings.Contains(out, "已重置") {
		t.Fatalf("reset 回显异常: %s", out)
	}

	// 重置后 head == 源
	if got, _ := repo.Resolve("zero"); got != srcHead {
		t.Fatalf("重置后应等于源: %s vs %s", got, srcHead)
	}
	// 内容也换了
	if _, err := repo.Read("zero", "b.md"); err != nil {
		t.Fatal("重置后应能看到源分支的内容")
	}

	// 旧版本必须被归档成 zero-reset-*，且指向重置前的提交
	refs, err := repo.Refs("refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	var archiveRef string
	for _, ref := range refs {
		if strings.HasPrefix(strings.TrimPrefix(ref, "refs/heads/"), "zero-reset-") {
			archiveRef = strings.TrimPrefix(ref, "refs/heads/")
		}
	}
	if archiveRef == "" {
		t.Fatalf("重置必须归档旧版本，实际分支: %v", refs)
	}
	if got, _ := repo.Resolve(archiveRef); got != preResetHead {
		t.Fatalf("归档分支应指向重置前的提交 %s，实际 %s", preResetHead, got)
	}
}

// reset 的边界：源与目标相同、缺分支、跨项目都必须报错。
func TestResetRejectsInvalidTargets(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/x.md -- x"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m x"); err != nil {
		t.Fatal(err)
	}

	if _, err := callMCP(t, srv, tok, "reset blackegg/me@zero from blackegg/me@zero"); err == nil {
		t.Fatal("源与目标相同应报错")
	}
	if _, err := callMCP(t, srv, tok, "reset blackegg/me@zero from blackegg/me@nosuch"); err == nil {
		t.Fatal("源分支不存在应报错")
	}
	if _, err := callMCP(t, srv, tok, "reset blackegg/me from blackegg/me@zero"); err == nil {
		t.Fatal("缺目标分支应报错")
	}
}

// 分叉是零字节的：对象总数不该因为 fork 增长。
func TestForkDoesNotCopyObjects(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		path := "blackegg/me@zero/f" + string(rune('a'+i)) + ".md"
		if _, err := callMCPWithVars(t, srv, tok, "write "+path+" -- $B",
			map[string]string{"B": "内容"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m bulk"); err != nil {
		t.Fatal(err)
	}

	countObjects := func() int {
		shas, err := repo.AllObjectSHAs()
		if err != nil {
			t.Fatalf("枚举对象失败: %v", err)
		}
		return len(shas)
	}
	before := countObjects()
	for i := 0; i < 3; i++ {
		if _, err := callMCP(t, srv, tok, "branch blackegg/me copy"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	after := countObjects()
	if after != before {
		t.Fatalf("fork 不该复制对象：%d → %d", before, after)
	}
}
