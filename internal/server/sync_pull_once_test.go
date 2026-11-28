package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 一次拉取里，整树校验不许做两遍。
//
// 起因（2026-11-28 实测，v0.2.18 三端）：`weft@main.record` 的拉取耗时
// 37.6 秒，而 3060 侧 `served` 只有 0~12ms、`missing` 为 0（正文一条都没补传）。
// 把日志摊开算时间差，37 秒里有 31 秒落在「一条日志都没有」的区间里。
//
// 其中一段的成因是：ingestOps 已经整树校验过一遍（Diff("", base) 走完
// 2.1 万条，再自算一棵期望树），pullOpsFromPeer 又校验了一遍。第二遍只在
// **补过正文之后**才有意义——补正文改变了落地状态；没补过，状态没变，
// 重比是纯重复。
//
// 这里用一次「正文都在本地」的差分拉取来钉它：op 引用的 blob 对端已有
// （从已有条目复制而来），所以 Missing 为空，第二遍必须不跑。
func TestRebaseVerifyRunsOnceWhenNothingWasFetched(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})
	_ = dstToken
	srcSrv := httptest.NewServer(src.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srcSrv.Close()

	write := func(path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, src, srcToken, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + path + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write("a.md", "同一份正文")

	client := &http.Client{Timeout: 30 * time.Second}
	// 第一趟：dst 什么也没有，会走整树快照，正文也要补——这趟允许校验两遍。
	if _, err := dst.pullOpsFromPeer(client, srcSrv.URL, srcToken, "peer-src", "blackegg/me", "zero"); err != nil {
		t.Fatalf("首次拉取: %v", err)
	}

	// 让 src 多出一个**引用已有正文**的路径：复制 a.md。
	// 这样 dst 那边 blob 已在本地，Missing 必为空。
	if _, err := callMCP(t, src, srcToken, "cp blackegg/me@zero:a.md --to blackegg/me@zero:b.md"); err != nil {
		t.Fatalf("cp: %v", err)
	}
	if _, err := callMCP(t, src, srcToken, "commit blackegg/me@zero -m copy"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	before := dst.verifyCalls.Load()
	res, err := dst.pullOpsFromPeer(client, srcSrv.URL, srcToken, "peer-src", "blackegg/me", "zero")
	if err != nil {
		t.Fatalf("第二次拉取: %v", err)
	}
	if len(res.Missing) != 0 {
		t.Fatalf("这趟不该缺正文（blob 是复制来的），实际缺 %d 条", len(res.Missing))
	}
	if got := dst.verifyCalls.Load() - before; got != 1 {
		t.Fatalf("没补正文时整树校验只该跑一遍（ingestOps 里那次），实际跑了 %d 遍", got)
	}
}

// 慢的拉取必须自己解释时间花在哪。
//
// 为什么这条要钉：37 秒那次事后追查，中间 31 秒一条日志都没有，
// 只能靠相邻两条记录的时间差去推——我因此连着推错三轮原因。
// 慢不可怕，慢得说不清才可怕。
func TestSlowPullExplainsItself(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	_ = mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})
	srcSrv := httptest.NewServer(src.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srcSrv.Close()

	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:a.md $BODY",
			"vars": map[string]string{"BODY": "正文"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 把阈值降到 0：任何一次拉取都算「慢」，日志因此必然出现。
	// 用变量而不是常量，就是为了让这条可断言——否则只能靠真等 5 秒，
	// 那在 CI 上既慢又不稳。
	old := slowPullThreshold
	slowPullThreshold = 0
	defer func() { slowPullThreshold = old }()

	logs := captureLogs(t, dst)
	if _, err := dst.pullOpsFromPeer(&http.Client{Timeout: 30 * time.Second}, srcSrv.URL, srcToken, "peer-src", "blackegg/me", "zero"); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "SLOW") {
		t.Fatalf("慢拉取必须打 SLOW 行，实际日志：\n%s", out)
	}
	// 分阶段必须真的分段：五个数字都在。
	for _, want := range []string{"receive", "apply+commit", "blobs+replay", "verify+record"} {
		if !strings.Contains(out, want) {
			t.Fatalf("SLOW 行缺少阶段 %q：\n%s", want, out)
		}
	}
}
