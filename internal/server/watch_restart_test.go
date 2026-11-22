package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// remind 必须是幂等的：重复登记同一棵树不能把基线推走。
//
// 为什么这条性命攸关：agent 扩展每次启动都会把配置里 watch 的树
// 重新 remind 一遍（anchorWatchedTrees）。如果 remind 每次都把基线
// 重置到「此刻」，那么 agent 停机期间对端写进来的东西会被当场抹平——
// status 永远报「没变化」，唤醒链就断在最需要它的那一刻。
//
// 实测背景（2026-11-22）：3060 上 watches.json 里 zero 有 4 条关注，
// 但它们的 baseline 每到 agent 重启就被推到最新，等于没有历史。
func TestRemindKeepsBaselineOnRepeat(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "watcher", Cap: "rwx"})

	// 1. 首次登记，记下基线
	out1, err := callMCP(t, srv, tok, "remind daily@zero")
	if err != nil {
		t.Fatalf("首次 remind: %v", err)
	}
	if !strings.Contains(out1, "from ") {
		t.Fatalf("首次 remind 应报告基线，实际: %s", out1)
	}

	// 2. 分支往前跑（模拟 agent 停机期间对端写了东西）
	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write daily@zero:new.md $BODY",
			"vars": map[string]string{"BODY": "停机期间的新内容"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit daily@zero -m advance"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 3. 扩展重启：再 remind 一次同一棵树
	out2, err := callMCP(t, srv, tok, "remind daily@zero")
	if err != nil {
		t.Fatalf("二次 remind: %v", err)
	}

	// 判据：基线必须没变（否则停机期间的推进被抹平）
	base1 := baselineFromRemind(t, out1)
	base2 := baselineFromRemind(t, out2)
	if base1 != base2 {
		t.Fatalf("重复 remind 把基线推走了：%s -> %s（停机期间的变化会被静默抹平）", base1, base2)
	}

	// 4. status 必须仍然报出这棵树动了
	statusOut, err := callMCP(t, srv, tok, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(statusOut, "MOVED") {
		t.Fatalf("基线保留后 status 应当报告 MOVED，实际没有:\n%s", statusOut)
	}
}

// remind 必须能显式重新锚定，否则「我已经处理完了，从现在开始算」无路可走。
func TestRemindReanchorResetsBaseline(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "watcher", Cap: "rwx"})

	out1, err := callMCP(t, srv, tok, "remind daily@zero")
	if err != nil {
		t.Fatalf("remind: %v", err)
	}
	base1 := baselineFromRemind(t, out1)

	// 真正推进分支：先写再提交（分支干净时 commit 什么也不做）。
	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write daily@zero:reanchor.md $BODY",
			"vars": map[string]string{"BODY": "为了推进"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit daily@zero -m advance2"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	out2, err := callMCP(t, srv, tok, "remind daily@zero --reanchor")
	if err != nil {
		t.Fatalf("remind --reanchor: %v", err)
	}
	base2 := baselineFromRemind(t, out2)
	if base2 == base1 {
		t.Fatalf("--reanchor 应当把基线推到当前头，但仍然是 %s", base1)
	}

	// 重新锚定后 status 不该再报 MOVED
	statusOut, _ := callMCP(t, srv, tok, "status")
	if strings.Contains(statusOut, "MOVED") {
		t.Fatalf("--reanchor 之后不该再报 MOVED:\n%s", statusOut)
	}
}

// zero-memcp 重启后，关注表必须原样回来——包括基线。
//
// 这条锁的是落盘：watches.json 在 <data>/system/ 下，启动时加载。
func TestWatchesSurviveServerRestart(t *testing.T) {
	srv, dataDir := newWatchTestServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "watcher", Cap: "rwx"})

	out1, err := callMCP(t, srv, tok, "remind daily@zero")
	if err != nil {
		t.Fatalf("remind: %v", err)
	}
	base1 := baselineFromRemind(t, out1)

	// 停机期间分支推进
	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write daily@zero:while-down.md $BODY",
			"vars": map[string]string{"BODY": "停机期间写入"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit daily@zero -m while-down"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 关掉再开（同一份 data 目录）
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	srv2, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatalf("重启: %v", err)
	}
	t.Cleanup(func() { srv2.Close() })

	// 重启后 status 必须仍认得这条关注，且因为头已经前进而报 MOVED
	statusOut, err := callMCP(t, srv2, tok, "status")
	if err != nil {
		t.Fatalf("重启后 status: %v", err)
	}
	if !strings.Contains(statusOut, "daily@zero") {
		t.Fatalf("重启后关注表丢了（watches.json 没被加载）:\n%s", statusOut)
	}
	if !strings.Contains(statusOut, "MOVED") {
		t.Fatalf("重启后应当报告停机期间的变化（基线 %s），实际:\n%s", shortSHA(base1), statusOut)
	}
}

// baselineFromRemind 从 "watching X from abc1234 …" 里取出基线 SHA。
//
// 只取紧跟 from 的第一个词：消息后面还可能跟着
// "(baseline kept; use --reanchor …)" 这类补充说明。
func baselineFromRemind(t *testing.T, out string) string {
	t.Helper()
	// 两种措辞：watching X from <sha> / re-anchored X at <sha>。
	i := strings.Index(out, " from ")
	marker := " from "
	if i < 0 {
		i = strings.Index(out, " at ")
		marker = " at "
	}
	if i < 0 {
		t.Fatalf("remind 输出里没有基线: %q", out)
	}
	fields := strings.Fields(out[i+len(marker):])
	if len(fields) == 0 {
		t.Fatalf("remind 输出里 from 后面没有内容: %q", out)
	}
	return fields[0]
}
