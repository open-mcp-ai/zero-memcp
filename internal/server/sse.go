package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Broadcaster 广播 SSE 事件。
type Broadcaster struct {
	mu          sync.RWMutex
	subscribers map[chan []byte]struct{}
}

func newBroadcaster() *Broadcaster {
	return &Broadcaster{subscribers: make(map[chan []byte]struct{})}
}

func (b *Broadcaster) subscribe() (<-chan []byte, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan []byte, 16)
	b.subscribers[ch] = struct{}{}
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subscribers, ch)
		close(ch)
	}
}

// Send 广播一条事件：event 类型 + 纯文本/JSON。
func (b *Broadcaster) Send(event, data string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
	for ch := range b.subscribers {
		select {
		case ch <- msg:
		default:
			// 慢客户端跳过，不阻塞其他订阅者
		}
	}
}

// handleEvents 提供 SSE 订阅通道，给 pi 插件做实时唤醒。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, cancel := s.bcast.subscribe()
	defer cancel()

	// 连上立刻推一条当前时钟作为握手
	curr := fmt.Sprintf(`{"clock":%d,"at":%d}`, s.Clock(), time.Now().Unix())
	_, _ = fmt.Fprintf(w, "event: clock\ndata: %s\n\n", curr)
	flusher.Flush()

	notify := r.Context().Done()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-notify:
			return
		case msg := <-ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			// 维持连接防中间代理超时
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// BroadcastMail 把一封新信广播给所有在线的客户端。
func (s *Server) broadcastMail(m Mail) {
	data, _ := json.Marshal(m)
	s.bcast.Send("mail", string(data))
}
