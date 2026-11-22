package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 双向同步不是「把拉取测试反着写一遍」：两个方向的水位彼此独立。
//
// 2026-11-22 真双实例狗粮抓到：B 从 A 拉到 seq 80 之后，B 自己写一条
// （本地日志 seq 1），推回 A 时却误用拉水位 80 截本地日志，导出空批次。
// 现象是 status 报同步成功、对端永远收不到，且日志没有任何错误。
func TestOpsSyncBidirectionalUsesIndependentWatermarks(t *testing.T) {
	a, aData := setupMountServer(t)
	b, bData := setupMountServer(t)
	tokA := mintRestricted(t, aData, auth.Claims{Sub: "zero", Cap: "rwx"})
	tokB := mintRestricted(t, bData, auth.Claims{Sub: "zero", Cap: "rwx"})

	write := func(s *Server, tok, path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, s, tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + path + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// 先在 A 上制造足够多的命令，让 B 的拉水位明显高于自己的写位点。
	for _, p := range []string{"a1.md", "a2.md", "a3.md"} {
		write(a, tokA, p, "from A: "+p)
	}
	srvA := httptest.NewServer(a.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srvA.Close()
	srvB := httptest.NewServer(b.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srvB.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	if _, err := b.pullOpsFromPeer(client, srvA.URL, tokA, "a", "blackegg/me", "zero"); err != nil {
		t.Fatalf("B 拉 A: %v", err)
	}
	if got := b.peerSeq("blackegg/me", "zero", b.resolvePeer("a")); got < 3 {
		t.Fatalf("B 的拉水位应 >=3，实际 %d", got)
	}

	// B 写自己的第一条命令，推回 A。
	write(b, tokB, "b1.md", "from B")
	if _, err := b.pushOpsToPeer(client, srvA.URL, tokA, "a", "blackegg/me", "zero"); err != nil {
		t.Fatalf("B 推 A: %v", err)
	}
	// 真正的不变量不是「等于 1」而是：推水位必须等于**自己本地日志**的位点，
	// 且与拉水位无关。
	//
	// （播种会让本地日志先有一条 opTree，所以具体数字不是 1。
	// 写死数字的话，每加一种「本地会自动产生命令」的机制，这个测试就假红一次。）
	localLog, err := b.opLogs.For("blackegg/me", "zero")
	if err != nil {
		t.Fatal(err)
	}
	outSeq := b.peerOutSeq("blackegg/me", "zero", "a")
	inSeq := b.peerSeq("blackegg/me", "zero", b.resolvePeer("a"))
	if outSeq != localLog.Last() {
		t.Fatalf("推水位应等于本地日志位点 %d，实际 %d", localLog.Last(), outSeq)
	}
	if outSeq == inSeq {
		t.Fatalf("推与拉的水位不应相同（in=%d out=%d）——相同说明两个方向又共用了同一个游标", inSeq, outSeq)
	}
	out, err := callMCP(t, a, tokA, "read blackegg/me@zero:b1.md")
	if err != nil {
		t.Fatalf("A 应读到 B 的反向写入: %v", err)
	}
	if !strings.Contains(out, "from B") {
		t.Fatalf("反向正文不对: %s", out)
	}

	// A 再写，B 再拉：两个方向互不干扰。
	write(a, tokA, "a4.md", "A again")
	if _, err := b.pullOpsFromPeer(client, srvA.URL, tokA, "a", "blackegg/me", "zero"); err != nil {
		t.Fatalf("B 再拉: %v", err)
	}
	out, err = callMCP(t, b, tokB, "read blackegg/me@zero:a4.md")
	if err != nil || !strings.Contains(out, "A again") {
		t.Fatalf("再次拉取失败: %v %s", err, out)
	}
}
