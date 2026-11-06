package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// 资源变更推送（MCP 2026-07-28 subscriptions/listen）。
//
// # 为什么是资源
//
// 一个记忆树（project@branch[/path]）本来就是资源：它有稳定的标识、
// 内容会演进、可以按 URI 订阅。用它承载变更事件不需要发明任何新概念，
// 载荷就是标准的一条 notifications/resources/updated。
//
// URI 形态：
//
//	memcp://<project>/<branch>              整个分支
//	memcp://<project>/<branch>/<path...>    子树
//
// # 与 months 前那套自定义 SSE 的区别
//
// 旧实现是第二个端点（/events）+ 自定义事件名 + 自定义鉴权语义。
// 现在只有 /mcp 一个入口：listen 是一个普通 JSON-RPC 请求，它的
// **响应体**就是事件流。鉴权、来源、错误处理全部与工具调用同源。
//
// # origin 为什么必须有
//
// 写入会推进分支头，推进会产生事件，事件会唤醒写入者——这是一个
// 可以自我维持的循环。默认情况下它是 bug（自己写的记忆唤醒自己，
// 白烧 token）；但它也可以是**特性**（写一条 TODO 就继续干活，
// 直到办完）。二者的区别只在客户端策略，服务端不改行为，只把
// 「谁写的」如实带上，由客户端决定要不要理自己。

// treeURI 把 project@branch[/path] 渲染成资源 URI。
//
// # 为什么项目名要转义
//
// 项目名本身就含斜杠（"BlackEggAI/zero"），而 URI 用斜杠当分隔符。
// 直接拼接会让 memcp://BlackEggAI/zero/main.record 解析成
// project="BlackEggAI", branch="zero", path="main.record"——全错位。
//
// 这个 bug 曾经藏得很深：生成与解析用同一套错误逻辑，两边错得一样，
// 字符串恰好相等，于是端到端「通过」。换成真实凭证才暴露（403），
// 因为 root 全放行掩盖了越界。
//
// 所以项目段做 percent-encoding：memcp://BlackEggAI%2Fzero/main.record
func treeURI(project, branch, path string) string {
	u := "memcp://" + url.PathEscape(project) + "/" + url.PathEscape(branch)
	if path != "" {
		u += "/" + strings.TrimPrefix(path, "/")
	}
	return u
}

// parseTreeURI 解析资源 URI，返回 project / branch / path。
func parseTreeURI(raw string) (project, branch, path string, err error) {
	if !strings.HasPrefix(raw, "memcp://") {
		return "", "", "", fmt.Errorf("unsupported resource scheme (want memcp://): %q", raw)
	}
	rest := strings.TrimPrefix(raw, "memcp://")
	if rest == "" {
		return "", "", "", fmt.Errorf("empty resource path: %q", raw)
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("resource URI needs memcp://<project>/<branch>: %q", raw)
	}
	if project, err = url.PathUnescape(parts[0]); err != nil {
		return "", "", "", fmt.Errorf("bad project segment in %q: %w", raw, err)
	}
	if project == "" {
		return "", "", "", fmt.Errorf("empty project in %q", raw)
	}
	if branch, err = url.PathUnescape(parts[1]); err != nil {
		return "", "", "", fmt.Errorf("bad branch segment in %q: %w", raw, err)
	}
	if branch == "" {
		return "", "", "", fmt.Errorf("empty branch in %q", raw)
	}
	if len(parts) == 3 {
		path = parts[2]
	}
	return project, branch, path, nil
}

// resourceEvent 是推给订阅者的一条变更。
type resourceEvent struct {
	URI string
	// Origin 是导致这次推进的写入方。客户端据此决定要不要理自己。
	Origin EventOrigin
	// Commit 是推进后的分支头，方便客户端直接 since 取差量。
	Commit string
	// Previous 是推进前的头。
	Previous string
}

// EventOrigin 描述「谁让这棵树动的」。
type EventOrigin struct {
	// Identity 是写入凭证对应的身份名。
	Identity string `json:"identity,omitempty"`
	// Session 是写入方声明的会话标识（由客户端在 put 时带上来）。
	Session string `json:"session,omitempty"`
	// Endpoint 是本机端点名（多机场景下区分 laptop / desktop）。
	Endpoint string `json:"endpoint,omitempty"`
}

// subscription 是一条活跃的 listen 流。
type subscription struct {
	id      string
	caller  Caller
	watches []string // 规范化后的 URI
	ch      chan resourceEvent
	closeMu sync.Once
	closed  chan struct{}
}

// resourceHub 管理活跃订阅并把变更扇出给关心的订阅者。
type resourceHub struct {
	mu     sync.RWMutex
	subs   map[string]*subscription
	nextID int
}

func newResourceHub() *resourceHub {
	return &resourceHub{subs: make(map[string]*subscription)}
}

func (h *resourceHub) add(caller Caller, watches []string) *subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	sub := &subscription{
		id:     "sub:" + strconv.Itoa(h.nextID),
		caller: caller,
		watches: watches,
		// 缓冲要够吞下突发：一次批量提交可能连着推多条。
		ch:     make(chan resourceEvent, 32),
		closed: make(chan struct{}),
	}
	h.subs[sub.id] = sub
	return sub
}

func (h *resourceHub) remove(sub *subscription) {
	h.mu.Lock()
	delete(h.subs, sub.id)
	h.mu.Unlock()
	sub.closeMu.Do(func() { close(sub.closed) })
}

// publish 把事件扇出给匹配的订阅者。
//
// 匹配规则是**前缀**：订阅分支则其下任何子树变更都算，订阅子树则只看
// 该子树。慢订阅者直接丢弃事件——它可以用 since 补，但不能拖住写入。
func (h *resourceHub) publish(ev resourceEvent) {
	h.mu.RLock()
	subs := make([]*subscription, 0, len(h.subs))
	for _, s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.RUnlock()

	for _, s := range subs {
		if !subMatches(s.watches, ev.URI) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			// 缓冲满：丢。客户端凭 Commit 调 since 补齐，事件可以丢、
			// 事实不能丢。
		}
	}
}

// subMatches 判断订阅的某条 URI 是否覆盖事件 URI。
func subMatches(watches []string, evURI string) bool {
	for _, w := range watches {
		if w == evURI {
			return true
		}
		// 订阅分支时，其下子树变更也算
		if strings.HasPrefix(evURI, w+"/") {
			return true
		}
	}
	return false
}

// serveSubscription 处理 subscriptions/listen：把响应体变成一个事件流。
//
// 协议要点（2026-07-28）：
//  1. 先发 notifications/subscriptions/acknowledged，告知服务端接受的 filter 子集；
//  2. 之后持续发 notifications/resources/updated；
//  3. 流正常结束时返回该请求的 result（空对象）。
//
// 客户端的两段式要求（先 ack 才算 open）意味着**ack 必须在任何事件之前**，
// 否则客户端会一直停在 opening 状态然后超时报错。
func (s *Server) serveSubscription(w http.ResponseWriter, r *http.Request, id any, params json.RawMessage, c Caller) {
	var req struct {
		Notifications struct {
			ResourcesListChanged bool     `json:"resourcesListChanged"`
			ResourceSubscriptions []string `json:"resourceSubscriptions"`
		} `json:"notifications"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			http.Error(w, "invalid params: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// 校验并规范化订阅 URI：越权/不存在的必须在这里拒绝，而不是
	// 订阅成功之后永远收不到事件（那是最难排查的一种静默失败）。
	var watches []string
	for _, raw := range req.Notifications.ResourceSubscriptions {
		project, branch, path, err := parseTreeURI(raw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !s.projectVisibleFor(c, project) {
			http.Error(w, fmt.Sprintf("resource %q is outside this credential's visible scope", raw),
				http.StatusForbidden)
			return
		}
		if !s.allowed(c, project, branch, configBitRead) {
			http.Error(w, s.denyReason(c, project, branch, configBitRead), http.StatusForbidden)
			return
		}
		watches = append(watches, treeURI(project, branch, path))
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// 不让中间代理缓冲：一个被缓冲的事件流等于没有事件流。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := s.resources.add(c, watches)
	defer s.resources.remove(sub)

	writeFrame := func(payload map[string]any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		// 标准 SSE 帧：event 缺省为 message，客户端按 JSON-RPC 解析。
		if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// 1. ack：回显被接受的 filter 子集。
	if !writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/subscriptions/acknowledged",
		"params": map[string]any{
			"notifications": map[string]any{
				"resourcesListChanged":   false,
				"resourceSubscriptions": watches,
			},
		},
	}) {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			// 服务关闭：发空 result 让客户端判定为 graceful 结束。
			writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
			return
		case <-sub.closed:
			writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
			return
		case ev := <-sub.ch:
			if !writeFrame(map[string]any{
				"jsonrpc": "2.0",
				"method":  "notifications/resources/updated",
				"params": map[string]any{
					"uri": ev.URI,
					// origin / commit / previous 走 _meta：
					// 协议为扩展数据留了位置，不污染标准字段。
					"_meta": map[string]any{
						"memcp/origin":   ev.Origin,
						"memcp/commit":   ev.Commit,
						"memcp/previous": ev.Previous,
					},
				},
			}) {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// publishTreeChange 在分支头推进后广播资源变更。
//
// origin 由调用方传入：谁触发了这次写入。写入路径必须如实传递，
// 否则客户端无法区分「我自己写的」和「别人写的」，自激循环就只能
// 靠节流硬压。
func (s *Server) publishTreeChange(project, branch, path, prev, commit string, origin EventOrigin) {
	if s.resources == nil {
		return
	}
	s.resources.publish(resourceEvent{
		URI:      treeURI(project, branch, path),
		Origin:   origin,
		Commit:   commit,
		Previous: prev,
	})
}

// resourceURIForResource 供 resources/list 用：把关注的分支渲染成 URI。
func resourcesListFor(watches []TreeWatch) []map[string]any {
	out := make([]map[string]any, 0, len(watches))
	for _, w := range watches {
		uri := treeURI(w.Project, w.Branch, w.Path)
		name := w.Project + "@" + w.Branch
		if w.Path != "" {
			name += "/" + w.Path
		}
		out = append(out, map[string]any{
			"uri":         uri,
			"name":        name,
			"description": "memory tree (project@branch[/path])",
			"mimeType":    "text/plain",
		})
	}
	return out
}

var _ = url.PathEscape
var _ = config.SemanticProvider{}
