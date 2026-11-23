package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 「对端删除」和「内容冲突」是两件完全不同的事，status 不许把它们说成一件。
//
// 实测（2026-11-23，rock64）：222 条删除留证被报成
//
//	222 unresolved work-layer conflict(s), local copy kept:
//	  record/...md: kept local b6ccfdd8, peer laptop offered
//
// 与事实完全相反——本地那份其实已经被解链了（删除优先是硬不变量）。
// 人看到「local copy kept」会以为内容还在，于是不去对象库里把它捞回来。
//
// 钉住的方向：
//  1. 删除留证必须自成一类，措辞是「被解链」，且不进「未解决冲突」计数。
//  2. 真冲突（双方内容不同、本地被保留）仍然照旧报「kept local」。
func TestDeleteWinsIsNotReportedAsLocalKept(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	const path = "2026-11-19/gone.md"

	// A 写一条（不 commit），推给 B，B 拿到一条**未提交**的本地副本
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + path + " $BODY",
			"vars": map[string]string{"BODY": "content"},
		},
	}); err != nil {
		t.Fatalf("A 写: %v", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if _, err := src.pushWorkToPeer(client, dstSrv.URL, dstToken, "dst", "blackegg/me", "zero", 1); err != nil {
		t.Fatalf("首轮同步: %v", err)
	}
	repoDst, _ := dst.projects.Get("blackegg/me")
	if _, found, _ := repoDst.Stat("zero", path); !found {
		t.Fatal("前提：B 应当先有这条未提交副本")
	}

	// A 删掉（不 commit），再同步：B 侧那份未提交副本会被墓碑解链
	repoSrc, _ := src.projects.Get("blackegg/me")
	if err := repoSrc.Delete("zero", path); err != nil {
		t.Fatalf("A 删: %v", err)
	}
	if _, err := src.pushWorkToPeer(client, dstSrv.URL, dstToken, "dst", "blackegg/me", "zero", 2); err != nil {
		t.Fatalf("二轮同步: %v", err)
	}

	// 墓碑必须已经落地
	if _, found, _ := repoDst.Stat("zero", path); found {
		t.Fatal("B 侧这条应当已被删除")
	}

	lines := dst.workConflictLines(dst.rootCaller())
	joined := strings.Join(lines, "\n")
	t.Logf("B 的 status 冲突段:\n%s", joined)

	// ① 不许把删除说成「保留本地」
	if strings.Contains(joined, "unresolved work-layer conflict") {
		t.Errorf("对端删除本地未提交副本**不是未解决冲突**：这件事已经决定了。"+
			"把它计入 unresolved 会让人以为有待裁决的东西。实际输出:\n%s", joined)
	}
	// ② 必须留下证据，并且说清楚本地那份已经没了
	if !strings.Contains(joined, "unlinked by a peer delete") {
		t.Errorf("对端删除必须留证并说清「已解链」，实际输出:\n%s", joined)
	}
	if strings.Contains(joined, "local copy kept") {
		t.Errorf("本地副本已被解链，输出里不许出现「local copy kept」——"+
			"那与事实相反，会让人不去对象库里捞回被删的 blob。实际输出:\n%s", joined)
	}
	if !strings.Contains(joined, path) {
		t.Errorf("留证里必须点名具体路径，实际输出:\n%s", joined)
	}
}
