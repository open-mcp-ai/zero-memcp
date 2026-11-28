package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 这一组钉的是「慢也要出声」（issue #14）。
//
// 起因是一次真实的误判：2026-11-28 01:21 之后同步流水静了 12 分钟，
// 我据此断定「22 条提交卡住推不出去」，查了一轮。真相是循环正在把
// 10,007 条推给 rock64——一次 12.5 分钟的操作，日志里一个字都没有。
//
// 两次教训各成一条断言：
//  1. 动手之前要说清楚要推多少、推给谁（否则「没动静」=「死掉了」）；
//  2. 耗时必须落进流水（否则事后只能靠相邻记录的时间差去猜，而猜错过）。
func TestSlowSyncAnnouncesItselfAndRecordsDuration(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})
	_ = srcToken

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()
	client := &http.Client{Timeout: 60 * time.Second}

	const project, branch = "blackegg/me", "zero"

	srcLogs := captureLogs(t, src)
	dstLogs := captureLogs(t, dst)

	// 造一批「大」内容：条数必须超过阈值，否则这次推送不该出声。
	repoSrc, err := src.projects.Ensure(project)
	if err != nil {
		t.Fatal(err)
	}
	big := batchLogThreshold + 20
	for i := 0; i < big; i++ {
		if err := repoSrc.Write(branch, fmt.Sprintf("bulk/%04d.md", i), []byte("x"), gitstore.ModeContent); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := repoSrc.Commit(branch, "bulk"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := src.pushOpsToPeer(client, dstSrv.URL, dstToken, "dst", project, branch); err != nil {
		t.Fatalf("推送大批次: %v", err)
	}

	// ① 推之前必须说了要推多少（"pushing N op(s)"）。
	if !strings.Contains(srcLogs.String(), "pushing") {
		t.Fatalf("大批次推送必须先在日志里说明，实际日志:\n%s", srcLogs.String())
	}
	// ② 推完必须记一条含耗时的完成行。
	if !strings.Contains(srcLogs.String(), "pushed") || !strings.Contains(srcLogs.String(), "in ") {
		t.Fatalf("大批次推送完成必须记耗时，实际日志:\n%s", srcLogs.String())
	}
	// ③ 收方也必须出声（否则「对端在忙着落地」也无从得知）。
	if !strings.Contains(dstLogs.String(), "receiving") {
		t.Fatalf("接收大批次时必须出声，实际日志:\n%s", dstLogs.String())
	}

	// ④ 流水里必须有非零耗时——这正是当初缺掉的那一项。
	recs := src.syncLog.snapshot().records
	var pushed *syncRecord
	for i := range recs {
		if recs[i].Dir == "push" && recs[i].Branch == branch {
			pushed = &recs[i]
		}
	}
	if pushed == nil {
		t.Fatal("没有找到 push 流水记录")
	}
	if pushed.Applied < big {
		t.Fatalf("推送记录里 applied=%d，应至少 %d", pushed.Applied, big)
	}
	if pushed.DurationMs <= 0 {
		t.Fatalf("duration_ms 必须反映真实耗时，实际 %d（字段声明了却没人填，见 issue #14）", pushed.DurationMs)
	}

	// ⑤ 小批次不该吵：再来一条小改动，日志里不能再出现 pushing 行。
	srcLogs.Reset()
	if err := repoSrc.Write(branch, "tiny.md", []byte("y"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repoSrc.Commit(branch, "tiny"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.pushOpsToPeer(client, dstSrv.URL, dstToken, "dst", project, branch); err != nil {
		t.Fatalf("推送小批次: %v", err)
	}
	if strings.Contains(srcLogs.String(), "pushing") {
		t.Fatalf("小批次不该打印大批次提示（会刷屏），实际日志:\n%s", srcLogs.String())
	}
}

// countBatchOps 决定「这批算不算大」，判错的两种后果都很糟：
// 漏报 = 十几分钟的静默；误报 = 每轮都刷屏。
func TestCountBatchOpsCountsPayloadNotJustOps(t *testing.T) {
	// 一条批量命令带 900 条路径：命令条数是 1，实际工作量是 900。
	// 若按条数判断，最大的一批反而被判成最小的一批。
	ops := []treeOp{
		{Kind: opTree, Batch: make([]opEntry, 900)},
		{Kind: opSet, Path: "a.md"},
	}
	if got := countBatchOps(ops); got != 901 {
		t.Fatalf("countBatchOps = %d, 期望 901（900 条载荷 + 1 条单命令）", got)
	}
	if !(countBatchOps(ops) >= batchLogThreshold) {
		t.Fatal("901 条必须被判成大批次")
	}
	if countBatchOps([]treeOp{{Kind: opSet, Path: "a.md"}}) >= batchLogThreshold {
		t.Fatal("单条命令不该被判成大批次")
	}
}

// captureLogs 把服务器的日志改道到内存，并在测试结束时还原。
func captureLogs(t *testing.T, s *Server) *logCatcher {
	t.Helper()
	c := &logCatcher{ch: make(chan string, 4096)}
	prev := s.logOut.Load()
	s.logOut.Store(&logSink{w: c})
	t.Cleanup(func() { s.logOut.Store(prev) })
	return c
}

// logCatcher 收日志行。
//
// 用通道而不是「互斥量 + 缓冲」：这个仓库有一条全局门禁禁止 sync.Mutex
// 字段（见 internal/guard），而日志从多个协程来，串行化只能靠通道。
// 门禁在这里起作用是好事——它连测试代码一起管，规则才不会被测试豁免掉。
type logCatcher struct {
	ch    chan string
	lines []string
}

func (c *logCatcher) Write(p []byte) (int, error) {
	select {
	case c.ch <- string(p):
	default: // 满了就丢：断言只关心「有没有出现某一行」
	}
	return len(p), nil
}

// drain 把已写入的行取回来（非阻塞）。
func (c *logCatcher) drain() {
	for {
		select {
		case line := <-c.ch:
			c.lines = append(c.lines, line)
		default:
			return
		}
	}
}

func (c *logCatcher) String() string {
	c.drain()
	return strings.Join(c.lines, "")
}

func (c *logCatcher) Reset() {
	c.drain()
	c.lines = nil
}
