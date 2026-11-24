package gittransport

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 慢但一直有字节在流动：总耗时即便远超静默窗口，也绝不该被掐断。
//
// 为什么这条必须通过：整轮时间预算（旧 90 秒）在 3060 慢但持续传 pack 的
// 场景下会一刀切误杀；流式活跃检测的判据是「字节流没流」，流了就能走完。
func TestStallTransportAllowsSlowContinuousStream(t *testing.T) {
	// 服务端每 40ms 发一小块，连续发 10 块（总耗时约 400ms）。
	// 静默窗口设为 150ms（总耗时 > 2×静默窗口）。
	// 如果用的是整轮预算，它在 150ms 就死了；用滑动窗口它能跑完全程。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		for i := 0; i < 10; i++ {
			time.Sleep(40 * time.Millisecond)
			_, _ = w.Write([]byte("chunk\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: stallTransport(150 * time.Millisecond),
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读流式体不应被掐断，实际错误: %v", err)
	}
	if got := strings.Count(string(body), "chunk\n"); got != 10 {
		t.Fatalf("应收到 10 块，实际收到 %d: %q", got, string(body))
	}
}

// 静默时间真正超标：连接必须准时被掐断并报错，绝不无限等。
//
// 为什么这条必须通过（2026-11-23 8 小时静默停摆的真因）：无限等等于
// 放弃故障检测；真正死掉的连接必须在静默窗口耗尽后出声。
func TestStallTransportAbortsOnDeadSilentConnection(t *testing.T) {
	// 服务端写了响应头之后假死（停顿 600ms 不再发任何字节）。
	// 静默窗口设为 120ms。必须在约 120ms 时读抛 i/o timeout。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(600 * time.Millisecond)
		_, _ = w.Write([]byte("too late\n"))
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: stallTransport(120 * time.Millisecond),
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("发起请求失败: %v", err)
	}
	defer resp.Body.Close()

	t0 := time.Now()
	_, err = io.ReadAll(resp.Body)
	elapsed := time.Since(t0)

	if err == nil {
		t.Fatal("假死连接应在静默超时后报错，实际读到了内容")
	}
	// 报错必须是 timeout
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("错误应是超时，实际: %v", err)
		}
	}
	// 掐断时间应在 120ms 附近（允许调度抖动），绝不能等满服务端的 600ms
	if elapsed > 400*time.Millisecond {
		t.Fatalf("掐断过晚：花了 %v，远超静默窗口 120ms（接近了服务端的 600ms）", elapsed)
	}
}
