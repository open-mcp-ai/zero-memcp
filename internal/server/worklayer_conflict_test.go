package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 普通改写的并发冲突保护：
// A 和 B 两台机器同时在本地改了同一条路径的内容，A 往 B 同步时：
//  1. B **绝对不能静默覆盖**本地的改动（被吃掉的一方毫无察觉是最贵的故障）；
//  2. B 必须保留本地版本，并把对端的改动记进冲突表；
//  3. status 必须清楚列出该冲突（点名路径、双方 shortSHA 与对端节点）。
func TestWorkLayerConcurrentRewriteReportsConflict(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	const targetPath = "2026-11-19/meeting.md"

	// 1. A 写了自己的版本
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + targetPath + " $BODY",
			"vars": map[string]string{"BODY": "版本 A：讨论了同步协议细节"},
		},
	}); err != nil {
		t.Fatalf("A 写: %v", err)
	}

	// 2. B 在本地同时也写了自己的版本（未 commit）
	if _, err := callMCPRaw(t, dst, dstToken, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + targetPath + " $BODY",
			"vars": map[string]string{"BODY": "版本 B：讨论了多索引架构方案"},
		},
	}); err != nil {
		t.Fatalf("B 写: %v", err)
	}

	// 3. A 推给 B
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := src.pushWorkToPeer(client, dstSrv.URL, dstToken, "dst", "blackegg/me", "zero", 1)
	if err != nil {
		t.Fatalf("推送报错: %v", err)
	}

	// 判据一：B 的回执里明确列出了该冲突
	if len(res.Conflicted) == 0 {
		t.Fatal("两边内容不同时，B 应当报告冲突，实际报告全被应用了（静默覆盖发作）")
	}
	if res.Conflicted[0].Path != targetPath {
		t.Fatalf("冲突路径不符: %+v", res.Conflicted)
	}
	t.Logf("成功捕获冲突: %+v", res.Conflicted[0])

	// 判据二：B 本地内容必须完好无损，没有被 A 覆盖
	repoDst, _ := dst.projects.Get("blackegg/me")
	entry, found, err := repoDst.Stat("zero", targetPath)
	if err != nil || !found {
		t.Fatalf("B 本地条目不见了: %v", err)
	}
	body, _ := repoDst.BlobBody(entry.Hash)
	if !strings.Contains(string(body), "版本 B") {
		t.Fatalf("B 的本地改动被 A 覆盖了！实际内容为:\n%s", string(body))
	}

	// 判据三：B 的 status 必须出声点名冲突
	statusOut, err := callMCP(t, dst, dstToken, "status")
	if err != nil {
		t.Fatalf("查 status: %v", err)
	}
	if !strings.Contains(statusOut, "unresolved work-layer conflict") || !strings.Contains(statusOut, targetPath) {
		t.Fatalf("status 必须出声报警，实际输出:\n%s", statusOut)
	}
	t.Logf("status 成功报警:\n%s", statusOut)
}
