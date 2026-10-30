package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 信箱：投递、查看、确认，全流程闭环。
func TestMailboxLifecycle(t *testing.T) {
	srv, dataDir := newTestServer(t, "")
	tok := mintToken(t, dataDir, "zero")

	// 1. 空信箱
	res, err := callMCP(t, srv, tok, "mailbox")
	if err != nil || !strings.Contains(res, "信箱为空") {
		t.Fatalf("空信箱应显示为空: %v, out: %s", err, res)
	}

	// 2. 投递一封点对点信件
	res, err = callMCP(t, srv, tok, "msg @qq 帮我看一眼那个告警")
	if err != nil || !strings.Contains(res, "已投递 #1") {
		t.Fatalf("投递失败: %v, out: %s", err, res)
	}

	// 3. 查信箱：应该有一封
	res, err = callMCP(t, srv, tok, "mailbox")
	if err != nil || !strings.Contains(res, "帮我看一眼那个告警") {
		t.Fatalf("信箱里没有刚才投递的信: %v, out: %s", err, res)
	}

	// 4. HTTP GET /mailbox 端点（pi 插件的热路径）
	// 信箱现在要求鉴权：它直接暴露跨端点投递的内容。
	req := httptest.NewRequest(http.MethodGet, "/mailbox", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.handleMailboxAPI(rec, req)
	var getResp struct {
		Count int `json:"count"`
		Mails []struct {
			Seq  int64  `json:"seq"`
			Body string `json:"body"`
		} `json:"mails"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("解析 /mailbox: %v", err)
	}
	if getResp.Count != 1 || getResp.Mails[0].Body != "帮我看一眼那个告警" {
		t.Fatalf("/mailbox 结果不对: %+v", getResp)
	}

	// 5. 确认该信
	res, err = callMCP(t, srv, tok, "mailbox ack 1")
	if err != nil || !strings.Contains(res, "已确认 1 封") {
		t.Fatalf("ack 失败: %v, out: %s", err, res)
	}

	// 6. 确认后信箱变空
	res, _ = callMCP(t, srv, tok, "mailbox")
	if !strings.Contains(res, "信箱为空") {
		t.Fatalf("ack 后信箱应为空，实际: %s", res)
	}
}

// 唤醒：SSE 订阅收到时钟递增广播。
func TestSSEBroadcastClock(t *testing.T) {
	srv, _ := newTestServer(t, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events", nil)

	// 开协程消费第一条握手事件
	done := make(chan string, 1)
	go func() {
		srv.handleEvents(rec, req)
	}()

	time.Sleep(30 * time.Millisecond) // 等连接建立
	// 递增时钟，应触发广播
	srv.bumpClock()

	// 从 recorder 读出收到的内容
	go func() {
		for i := 0; i < 20; i++ {
			if strings.Contains(rec.Body.String(), "event: clock") {
				done <- rec.Body.String()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		done <- ""
	}()

	select {
	case body := <-done:
		if !strings.Contains(body, "event: clock") {
			t.Fatalf("未收到 SSE 时钟广播: %s", body)
		}
	case <-time.After(time.Second):
		t.Fatal("等待 SSE 广播超时")
	}
}
