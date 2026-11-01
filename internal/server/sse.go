package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const dataPrefix = "data: "

// Broadcaster 把服务端事件扇出给所有在线的 SSE 订阅者。
//
// 它只是传输层：事件该不该给某个人看见，由 handleEvents 在写出之前判定。
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

// handleEvents 提供 SSE 通道。
//
// 只推送调用者通过 remind 锚定的树变更——树路径就是订阅语义：
// 没锚定的人不该知道那棵树动过，而不是收到后自己忽略。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}

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
			if !s.eventAllowed(string(msg), caller) {
				continue
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// eventAllowed 判定一条已序列化的 SSE 帧能否发给某个身份。
//
// 只有 clock 与 tree 两类事件存在。tree 事件的放行规则：
// 调用者必须关注过对应的树路径，且对该项目/分支有读权限。
// 关注关系就是授权——没关注的人不该知道那棵树动过。
func (s *Server) eventAllowed(frame string, caller Caller) bool {
	switch {
	case strings.HasPrefix(frame, "event: clock\n"):
		return true
	case strings.HasPrefix(frame, "event: tree\n"):
		body := strings.TrimPrefix(frame, "event: tree\n")
		if !strings.HasPrefix(body, dataPrefix) {
			return false
		}
		payload := strings.TrimPrefix(body, dataPrefix)
		payload = strings.TrimSuffix(payload, "\n\n")
		var ev TreeEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return false
		}
		if !s.treeWatch.Matches(caller.Identity, ev) {
			return false
		}
		return s.allowed(caller, ev.Project, ev.Branch, configBitRead)
	default:
		return false
	}
}

// broadcastTreeEvent 在 tree_events.go 里定义。
