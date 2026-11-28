package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// status 报的「这一轮传了几条」必须反映真实传输，不能恒为 0。
//
// 踩到的坑（2026-11-25）：noteOutOps 被插进了零传输的早退分支里，
// 于是每条分支最后都被覆盖成 0——写了一条新记忆、日志里明明有
// 「1 delta op(s)」、对端也收到了，status 却报 in sync (0 op)。
// 这种「显示与事实不符」最误导人：看 status 会以为同步什么都没做。
//
// 这个测试走真实的 push 路径，断言：
//
//	第一轮：有新内容 → lastOutOps 必须等于实际推的命令数（>0）
//	第二轮：无变化   → lastOutOps 必须为 0
func TestStatusReportsActualTransferCount(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	const project, branch, peer = "blackegg/me", "zero", "peer"

	client := &http.Client{Timeout: 15 * time.Second}

	// 先推一轮把基础内容送过去，此时对端从空开始。
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write " + project + "@" + branch + ":base.md $BODY",
			"vars": map[string]string{"BODY": "base"},
		},
	}); err != nil {
		t.Fatalf("写 base: %v", err)
	}
	if _, err := src.pushOpsToPeer(client, dstSrv.URL, dstToken, peer, project, branch); err != nil {
		t.Fatalf("首轮推: %v", err)
	}

	// 再写一条：这一轮必须有传输，报数不能是 0。
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write " + project + "@" + branch + ":second.md $BODY",
			"vars": map[string]string{"BODY": "second"},
		},
	}); err != nil {
		t.Fatalf("写 second: %v", err)
	}
	if _, err := src.pushOpsToPeer(client, dstSrv.URL, dstToken, peer, project, branch); err != nil {
		t.Fatalf("第二轮推: %v", err)
	}
	got, ran := src.workSync.lastOutOps(project, branch, peer)
	if !ran {
		t.Fatal("推过之后必须记下这一轮传了几条；没记的话 status 只能报 0 或猜")
	}
	if got == 0 {
		t.Fatal("这一轮确实推了新内容（对端从 1 条变 2 条），报数却是 0——" +
			"说明报数被零传输路径覆盖了")
	}
	t.Logf("有变化的一轮：%d op(s)", got)

	// 第三轮：内容没变，必须报 0。
	if _, err := src.pushOpsToPeer(client, dstSrv.URL, dstToken, peer, project, branch); err != nil {
		t.Fatalf("第三轮推: %v", err)
	}
	if got, _ := src.workSync.lastOutOps(project, branch, peer); got != 0 {
		t.Fatalf("内容一致的一轮应当报 0 op，实际 %d", got)
	}
}
