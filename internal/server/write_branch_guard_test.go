package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 往不存在的分支上写：三档行为各钉一条。
//
// 起因（2026-11-28 自狗粮抓到的真 bug）：
//
//	put blackegg/me@recrod $T   ← 打错一个字母
//	→ ok blackegg/me/2026-11-28/…-1-hub.md (blackegg/me@recrod:…)
//
// 内容落进了一条新分支，而那条分支：trees 看不见、同步配置里没有它所以
// 永远推不出去、sync 视图不显示、git 层面 branch -a 是空的（只活在 oplog
// 里）。用户以为写进去了，实际写进一个不备份、不可见、不同步的黑洞。
//
// 既有契约「显式 @branch 必须被尊重，不存在就建」不能破（跨分支写入靠它），
// 所以分三档：拼错 → 拒绝并给出「你是指 X 吗」；真新分支 → 放行但当场标注；
// 首次写入 → 放行。
func TestMisspelledBranchIsRefused(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*"}},
	})

	// 先写一条，让 zero 这条分支存在。
	if _, err := callMCP(t, srv, tok, "put blackegg/me@zero -- 第一条"); err != nil {
		t.Fatal(err)
	}

	// 打错一个字母。
	_, err := callMCP(t, srv, tok, "put blackegg/me@zeroo -- 打错分支名")
	if err == nil {
		t.Fatal("拼错的分支名必须被拒绝，实际写入成功——内容会落进不备份、不可见的黑洞")
	}
	msg := err.Error()
	if !strings.Contains(msg, "did you mean") || !strings.Contains(msg, "zero") {
		t.Fatalf("拒绝时必须给出「你是指 X 吗」，实际：%v", err)
	}
}

// 真正的新分支要放行，但返回值必须当场说明它不在任何同步列表里。
func TestGenuinelyNewBranchIsAllowedButAnnounced(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*"}},
	})
	if _, err := callMCP(t, srv, tok, "put blackegg/me@zero -- 第一条"); err != nil {
		t.Fatal(err)
	}

	out, err := callMCP(t, srv, tok, "put blackegg/me@chat -- 真新分支")
	if err != nil {
		t.Fatalf("真正的新分支必须放行（跨分支写入依赖它）：%v", err)
	}
	if !strings.Contains(out, "NEW branch") {
		t.Fatalf("新分支必须在返回值里当场标注（它不在任何同步列表里），实际：%q", out)
	}

	// 已存在的分支不该被标。
	out2, err := callMCP(t, srv, tok, "put blackegg/me@chat -- 再写一条")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "NEW branch") {
		t.Fatalf("已存在的分支不该被标成新分支：%q", out2)
	}
}

// 拼错检测必须看得见「刚写、还没提交」的分支。
//
// 只用 repo.Branches() 会漏掉它们（没有 ref，只活在脏层里）——
// 实测就是这么漏的：先 put 到某条分支，再 put 到它的近似拼写，
// Branches() 里没有那条分支，于是被当成首次写入放行。
//
// 用 onlydirty 这种「不可能被脚手架预建」的名字：如果拿 zero 来测，
// 而 zero 恰好在建项目时就存在，脏层被遮住，测试会假绿。
func TestTypoCheckSeesUncommittedBranches(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*"}},
	})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}

	// 只写不提交：这条分支只存在于脏层。
	if _, err := callMCP(t, srv, tok, "put blackegg/me@onlydirty -- 未提交的第一条"); err != nil {
		t.Fatal(err)
	}
	// 先确认这条分支确实不在 refs 里（否则这个测试测不到东西）。
	for _, b := range repo.Branches() {
		if b == "onlydirty" {
			t.Fatalf("onlydirty 已进 refs，这个测试测不到「脏层分支」的情况")
		}
	}
	pending := false
	for _, b := range repo.PendingRefs() {
		if b == "onlydirty" {
			pending = true
		}
	}
	if !pending {
		t.Fatalf("onlydirty 应出现在 PendingRefs 里，实际: %v", repo.PendingRefs())
	}

	if _, err := callMCP(t, srv, tok, "put blackegg/me@onlydirt -- 打错"); err == nil {
		t.Fatal("脏层里的分支也要参与拼错检测，实际放行了——内容会进一条没有 ref、不同步的分支")
	}
}
