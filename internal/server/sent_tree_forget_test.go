package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 推送只要没走到成功，就必须作废「我记得发给对端的那棵树」。
//
// 踩到的坑（2026-11-25，冷备份静默缺内容）：
//
//	rock64 的 main.dev 少了整整 1107 条，抽查路径读不到，而本机 read 正常。
//	本机一直报 in sync (0 op)。
//
// 根因：导出器在**发送之前**就记了 sentTree（它只负责算批，不知道成败），
// 而下一轮拿它当「对端手上是哪棵树」算差分。于是补正文阶段任何一条早退
// （读本地 blob 失败 / 上传 blob 失败 / 补完仍报 missing）都会让 sentTree
// 留在那里，下一轮得出「差 0 条」→ 报同步完成 → 对端永远收不到。
//
// 这个测试让推送必然失败（对端不存在），断言 sentTree 被清掉。
func TestPushFailureForgetsSentTree(t *testing.T) {
	src, srcData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	_ = srcToken

	const project, branch, peer = "blackegg/me", "zero", "peer"

	// 先造出内容，让导出器有东西可发。
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write " + project + "@" + branch + ":a.md $BODY",
			"vars": map[string]string{"BODY": "content"},
		},
	}); err != nil {
		t.Fatalf("写: %v", err)
	}

	// 指向一个必然连不上的地址：推送在传输层就失败。
	client := &http.Client{Timeout: 2 * time.Second}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // 关掉 → 连接被拒

	if _, err := src.pushOpsToPeer(client, dead.URL, "tok", peer, project, branch); err == nil {
		t.Fatal("对端已关闭，推送必须报错")
	}

	// 关键断言：失败之后不许留下「发过」的记忆。
	if st := src.workSync.sentTreeOf(project, branch, src.resolvePeer(peer)); st != "" {
		t.Fatalf("推送失败却留着 sentTree=%s——下一轮会据此算出「差 0 条」，"+
			"对端永远收不到内容，而且两边都不报错", st)
	}
}
