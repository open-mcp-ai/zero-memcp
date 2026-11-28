package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 拉取不得删掉「本地已提交、但还没推给对端」的内容。
//
// 真实事故（2026-11-26，三节点生产环境）：笔记本写入 41 个切片并把脏层
// 折叠成 commit（这是推送前的正常动作），紧接着按「先拉后推」从 hub-3060
// 拉了一轮。对端那时还没收到这批内容，于是它的树里没有这些路径——
// 接收侧把「对端没有」当成「对端已删除」，把刚写下的内容打了删除墓碑。
// 同类事件在 synclog 里累计 347 个文件（全部来自 hub-3060）。
//
// 这条不变式的形状：
//
//	对端没有某个路径 ≠ 对端删除了它。
//	它只说明对端手上没有——而「本地有、对端没有」的正常成因是
//	本地刚写完还没推出去（先拉后推的必然窗口）。
func TestPullMustNotDeleteLocallyCommittedContent(t *testing.T) {
	a, aDir := newWatchTestServer(t)
	b, bDir := newWatchTestServer(t)
	tokA := mintRestricted(t, aDir, auth.Claims{Sub: "zero", Name: "a", Cap: "rwx"})
	tokB := mintRestricted(t, bDir, auth.Claims{Sub: "zero", Name: "b", Cap: "rwx"})

	// A 写入并提交：这正是同步循环在推送前做的事。
	if _, err := callMCPRaw(t, a, tokA, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write daily@zero:survive.md $BODY",
			"vars": map[string]string{"BODY": "本地刚写完，还没推给对端"},
		},
	}); err != nil {
		t.Fatalf("A write: %v", err)
	}
	if _, err := callMCP(t, a, tokA, "commit daily@zero"); err != nil {
		t.Fatalf("A commit: %v", err)
	}

	tsB := httptest.NewServer(b.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer tsB.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 先拉后推：A 从还是空白的 B 拉一轮。
	res, err := a.pullOpsFromPeer(client, tsB.URL, tokB, "b", "daily", "zero")
	if err != nil {
		t.Fatalf("A pull from B: %v", err)
	}
	if res.Deleted > 0 {
		t.Logf("拉取报告删除了 %d 个文件（不应发生）", res.Deleted)
	}

	out, rerr := callMCP(t, a, tokA, "read daily@zero:survive.md")
	if rerr != nil {
		t.Fatalf("A 拉取后读不到自己刚写的文件：%v —— 对端没有≠对端删过", rerr)
	}
	if !strings.Contains(out, "还没推给对端") {
		t.Fatalf("A 拉取后内容变了：%q", out)
	}
}
