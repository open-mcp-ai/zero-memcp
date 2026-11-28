package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 当一条分支只有工作层改动（从未 commit 过）时：
//
// remind 不能报 "branch not found"——工作层是真实数据，不是空气。
// status 必须能看出这棵树正在活动，而不是看起来像配置写错。
//
// 实测故障（2026-11-22 狗粮）：
// A 写工作层（未 commit），B 收到并落盘、索引建立、read 读得到，
// 但 B 上的 status 却显示 `watched: blackegg/me@main [branch not found]`。
// 因为 watch.go 只调 repo.Resolve（读 Git ref），完全瞎掉了。
func TestRemindSeesUncommittedWorkLayer(t *testing.T) {
	srv, data := setupMountServer(t)
	token := mintRestricted(t, data, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 1. 盯住尚未 commit 的分支
	if _, err := callMCP(t, srv, token, "remind blackegg/me@uncommitted"); err != nil {
		t.Fatalf("remind 报错: %v", err)
	}

	// 2. 写一条工作层记忆（不 commit）
	if _, err := callMCPRaw(t, srv, token, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@uncommitted:test.md $BODY",
			"vars": map[string]string{"BODY": "未提交内容"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 3. status 绝不能报 "branch not found"
	statusOut, err := callMCP(t, srv, token, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if strings.Contains(statusOut, "[branch not found]") {
		t.Fatalf("工作层明明有数据，status 却报 [branch not found]（盲信 Git ref，看不见工作层）:\n%s", statusOut)
	}
	if !strings.Contains(statusOut, "uncommitted") {
		t.Fatalf("status 应当报告该分支的工作层状态:\n%s", statusOut)
	}
}

// 分支已有 commit，之后工作层又进了新改动（未 commit）：
// remind 必须能报告它「有变化」，不能因为头 SHA 没变就装作没事。
func TestRemindSeesWorkLayerChangesOnCommittedBranch(t *testing.T) {
	srv, data := setupMountServer(t)
	token := mintRestricted(t, data, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 初始提交
	if _, err := callMCPRaw(t, srv, token, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@main:init.md $BODY",
			"vars": map[string]string{"BODY": "初始"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, token, "commit blackegg/me@main -m init"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// remind 锚定基线
	if _, err := callMCP(t, srv, token, "remind blackegg/me@main"); err != nil {
		t.Fatalf("remind: %v", err)
	}

	// 此时 status 应当是平的（没变化）
	status1, _ := callMCP(t, srv, token, "status")
	if strings.Contains(status1, "HAS ADVANCED") || strings.Contains(status1, "work-layer modified") {
		t.Fatalf("刚锚定不该报变化:\n%s", status1)
	}

	// 写入一条工作层新记忆（不 commit）
	if _, err := callMCPRaw(t, srv, token, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@main:new.md $BODY",
			"vars": map[string]string{"BODY": "新工作层条目"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// status 必须能看出工作层动了，不能因为 commit SHA 没变就装看不见
	status2, _ := callMCP(t, srv, token, "status")
	if !strings.Contains(status2, "work layer modified") && !strings.Contains(status2, "uncommitted") {
		t.Fatalf("工作层新增了条目，status 却完全没反应（因为只比分支头 SHA）:\n%s", status2)
	}
}
