package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// pending 必须如实列出「上次 commit 之后写了什么」，一条不漏。
//
// 这是归纳流程的第一步：调用方拿到清单才知道该读哪几篇。
// 没有它就只能 ls 猜或者全量重读整条分支。
func TestPendingListsUncommittedChanges(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 干净分支：应当报告「没有待提交」
	out := mustCall(t, srv, tok, "pending blackegg/me@zero")
	if !strings.Contains(out, "nothing pending") {
		t.Fatalf("空分支应报告 nothing pending，实际：%q", out)
	}

	// 写两篇，但不 commit
	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 第一篇"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 第二篇"); err != nil {
		t.Fatal(err)
	}

	out = mustCall(t, srv, tok, "pending blackegg/me@zero")
	if !strings.Contains(out, "2 pending change(s)") {
		t.Fatalf("应列出 2 条待提交改动，实际：%q", out)
	}
	// 清单里必须有可直接 read 的规范地址（project@branch:path）
	if !strings.Contains(out, "blackegg/me@zero:") {
		t.Fatalf("清单应给规范地址，实际：%q", out)
	}

	// commit 之后清单必须清空
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m done"); err != nil {
		t.Fatal(err)
	}
	out = mustCall(t, srv, tok, "pending blackegg/me@zero")
	if !strings.Contains(out, "nothing pending") {
		t.Fatalf("提交后应无待提交，实际：%q", out)
	}
}

func mustCall(t *testing.T, srv *Server, tok, cmd string) string {
	t.Helper()
	res, err := callMCP(t, srv, tok, cmd)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return res
}

// write 必须能改已有文件、能新建文件——这是「让调用方维护自己的知识结构」
// 的前提。它曾经硬性要求 root，等于把「改 SKILL.md / 加一个新主题文件」
// 锁给了管理员。
//
// 能力边界：有某条分支的 w 权限 = 能改这条分支上任何文件。
// 早先 w 只够 append 新记忆（put 生成新名字，覆盖不了已有文件）。
func TestWriteCanOverwriteAndCreate(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 先种一篇
	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 原始内容"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}

	// 新建一个多行文件（归纳时「发现新主题就加一篇」就靠这个）。
	//
	// 正文必须走 payload 原子槽：termcp 的 DSL 按换行切指令，
	// 裸多行正文会被当成第二条指令（`unknown directive: 正文`）。
	// 这也是真实调用方的用法——正文永远通过变量传，不拼在语法行里。
	body := "# 新主题\n\n第一段。\n第二段。\n"
	out, werr := callMCPVars(t, srv, tok, "blackegg/me@zero:07-new-topic.md", body)
	if werr != nil {
		t.Fatalf("新建文件失败：%v", werr)
	}
	if !strings.Contains(out, "written to") {
		t.Fatalf("新建文件失败：%q", out)
	}

	got := mustCall(t, srv, tok, "read blackegg/me@zero:07-new-topic.md")
	if !strings.Contains(got, "第二段") {
		t.Fatalf("多行正文没有完整落盘：%q", got)
	}

	// 覆盖同一个文件
	out, werr = callMCPVars(t, srv, tok, "blackegg/me@zero:07-new-topic.md", "# 改过的主题")
	if werr != nil {
		t.Fatalf("覆盖已有文件失败：%v", werr)
	}
	if !strings.Contains(out, "written to") {
		t.Fatalf("覆盖已有文件失败：%q", out)
	}

	got = mustCall(t, srv, tok, "read blackegg/me@zero:07-new-topic.md")
	if !strings.Contains(got, "改过的主题") {
		t.Fatalf("覆盖后读到的不是新内容：%q", got)
	}
}
