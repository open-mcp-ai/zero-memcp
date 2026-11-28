package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gittransport"
)

// 对端变更订阅：把「等下一次轮询」换成「对端说它那边有新东西，现在就拉」。
//
// # 为什么需要这一跳
//
// kickSync 只在**本机写入**时触发（opproduce.go 每条写入调一次）。于是
// 反方向没有任何触发点：上游写了内容，下游要等 [[sync]].interval（默认
// 5m）的定时器才知道。备份其实没坏，只是慢了最多五分钟，而这个「慢」
// 在 status 上完全看不出来——同步日志写着 in sync，水位也对得上，
// 只是时钟还没走到。
//
// # 为什么复用 subscriptions/listen，不自造推送通道
//
// resource_events.go 已经实现了 MCP 的 subscriptions/listen：一个普通
// JSON-RPC 请求，响应体是 SSE 事件流，鉴权与 /mcp 同源，事件里带
// origin / commit。再自造一套推送协议等于多一份鉴权语义要维护，
// 而那正是上一版自造 SSE 被删掉的原因。
//
// # 为什么订阅不是正确性来源
//
// 事件只负责「快」。连接断了、事件丢了，水位仍然在持久化状态里，
// 定时器仍然会兜底。所以这里的一切失败都只降级为「退回轮询」，
// 绝不改变同步语义——订阅挂了不该影响任何一条已同步的内容。
//
// # 为什么跳过自己造成的事件
//
// 推送成功后对端会落地并广播，本机收到那条事件再 kick 一次，就是
// 空转一轮：拉到的是一批 0 条。origin.Endpoint 是本机端点名时跳过。
// 注意只在能够确认「这是本机推的」时跳过：origin 缺失或不是本机，
// 一律照常 kick——少 woke 一次只是慢一点，误跳过就是真漏。

const (
	// peerSubMinBackoff 是断线后的首轮重连间隔。
	//
	// 不能是 0：对端进程正在重启时会瞬间打出一串连接。也不能太长：
	// 这条连接存在的全部意义就是「快」。
	peerSubMinBackoff = 2 * time.Second
	// peerSubMaxBackoff 是退避上限。退到 1 分钟也就比默认轮询
	// 快 5 倍，再长就等于把这一跳废掉了。
	peerSubMaxBackoff = time.Minute
	// peerSubStableAfter 是「这条连接算稳定过」的门槛：
	// 活过这个长度再断开，退避重置回最小值。
	// 否则对端每隔几秒断一次时，退避会一路涨到上限再也降不下来。
	peerSubStableAfter = 30 * time.Second
)

// peerSubUnsupported 表示对端明确拒绝了这个订阅请求
// （协议不支持 / 无权限 / 参数不合法）。这类失败重试没有意义，
// 必须停下来并点名，而不是无限重连刷日志。
type peerSubUnsupported struct {
	remote string
	status int
	body   string
}

func (e *peerSubUnsupported) Error() string {
	return fmt.Sprintf("peer %s does not accept subscriptions/listen (HTTP %d): %s",
		e.remote, e.status, strings.TrimSpace(e.body))
}

// shouldSubscribe 决定要不要为一条远端起订阅连接。
//
// 默认判据（没写 subscribe 时）：工作层对端 + auto_sync + 会拉内容。
// push_only 的备份节点不订阅——我们从不从它拉，它那边有新内容也与我们无关。
func shouldSubscribe(rm config.Remote) bool {
	if rm.Subscribe != nil {
		return *rm.Subscribe
	}
	if !rm.AutoSync || rm.PushOnly || rm.FetchOnly {
		return false
	}
	if !isWorkPeerURL(rm.URL) {
		return false
	}
	return len(rm.Pull) > 0 || len(rm.Push) > 0
}

// peerSubscribeURIs 把配置里的分支白名单转成订阅 URI。
//
// 订阅是「对端那条分支有变化」，不是「给我它的内容」——所以列全
// 配置里会碰的分支即可；同一条分支重复出现只留一个。
func peerSubscribeURIs(project string, rm config.Remote) []string {
	branches := rm.Pull
	if len(branches) == 0 {
		// Pull 为空会回落到 Push（见 config.Remote 注释），
		// 所以订阅面与真正的拉取面一致。
		branches = rm.Push
	}
	seen := make(map[string]bool, len(branches))
	out := make([]string, 0, len(branches))
	for _, br := range branches {
		br = strings.TrimSpace(br)
		if br == "" {
			continue
		}
		uri := treeURI(project, br, "")
		if seen[uri] {
			continue
		}
		seen[uri] = true
		out = append(out, uri)
	}
	return out
}

// isSelfOrigin 判断一条事件是不是「本机推过去的内容被对端广播回来」。
//
// 这里曾经写成拿 Origin.Endpoint 跟本机端点名比——**永远不会相等**，
// 所以一次也没生效过。原因是对端落地本机推送时，两个字段填的是：
//
//	Identity = "peer:<本机端点名>"   ← 谁发来的命令
//	Endpoint = <对端自己的端点名>     ← 谁落的地
//
// （见 opapply.go 与 syncwork.go 里 publishTreeChange 的调用。）
// 所以判据只能看 Identity。看错的后果不是报错，而是每一轮推送
// 都换来对端一条事件、本机一次空转的拉取——静默地多跑一倍轮次。
//
// 判不出来时一律不跳过：少 woke 一次只是慢一点，误跳过是真漏。
func isSelfOrigin(o EventOrigin, self string) bool {
	if self == "" {
		return false
	}
	return o.Identity == "peer:"+self
}

// startPeerSubscriptions 给每个符合条件的远端起一条订阅协程。
//
// 与 startSyncLoop 同生命周期：订阅只负责把 runner 的 kick 打得勤一点，
// 停掉同步循环之后再订阅就是白连。
func (s *Server) startPeerSubscriptions() {
	cf := s.cfgFilePtr.Load()
	if cf == nil {
		return
	}
	for _, rm := range cf.Remotes {
		if !shouldSubscribe(rm) {
			continue
		}
		uris := peerSubscribeURIs(rm.Project, rm)
		if len(uris) == 0 {
			continue
		}
		rm := rm
		go s.peerSubscriptionLoop(rm, uris)
	}
}

// peerSubscriptionLoop 维持一条订阅连接，直到服务关闭或对端明确拒绝。
func (s *Server) peerSubscriptionLoop(rm config.Remote, uris []string) {
	backoff := peerSubMinBackoff
	for {
		select {
		case <-s.done:
			return
		default:
		}

		started := time.Now()
		err := s.streamPeerEvents(rm, uris)

		var unsupported *peerSubUnsupported
		if errors.As(err, &unsupported) {
			// 协议层拒绝：重试解决不了，说清楚然后退出这一条。
			// 同步本身不受影响——定时器还在。
			s.logf("op-sync: %v; falling back to polling for %s", unsupported, rm.Project)
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			s.logf("op-sync: subscription to %s (%s) ended: %v; reconnecting in %s",
				rm.Name, rm.Project, err, backoff.Round(time.Second))
		}
		select {
		case <-s.done:
			return
		case <-time.After(backoff):
		}

		if time.Since(started) >= peerSubStableAfter {
			backoff = peerSubMinBackoff
		} else if backoff < peerSubMaxBackoff {
			backoff *= 2
			if backoff > peerSubMaxBackoff {
				backoff = peerSubMaxBackoff
			}
		}
	}
}

// streamPeerEvents 开一条 subscriptions/listen 流，每收到一条资源变更
// 通知就 kick 一次同步。
//
// 返回 nil 表示流被正常关闭（对端发了 result 帧），调用方据此重连；
// 返回 *peerSubUnsupported 表示对端拒绝，调用方应当停止。
func (s *Server) streamPeerEvents(rm config.Remote, uris []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 服务关闭时把连接一起断掉，否则 Close 会等这条长连接超时。
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	token, err := s.optionalSecret(rm.HTTPToken, "http_token")
	if err != nil {
		return err
	}

	// 长连接专用客户端：整请求不能有总预算（Timeout 必须为 0），
	// 否则到点必然被砍掉，表现出来就是「每 20 秒断一次」。
	client, err := gittransport.NewSyncClient(gittransport.TLSConfig{
		CAPath: rm.TLSCA,
		Pin:    rm.TLSPin,
	})
	if err != nil {
		return err
	}
	client.Timeout = 0

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "subscriptions/listen",
		"params": map[string]any{
			"notifications": map[string]any{
				"resourceSubscriptions": uris,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(rm.URL, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return &peerSubUnsupported{remote: rm.Name, status: resp.StatusCode, body: string(snippet)}
		}
		// 5xx 可能只是对端正在重启，值得重试。
		return fmt.Errorf("subscriptions/listen returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	s.logf("op-sync: subscribed to %s (%s) for %d branch(es)", rm.Name, rm.Project, len(uris))

	self := s.instanceID()
	sc := bufio.NewScanner(resp.Body)
	// SSE 帧里可能有一整条通知（含 _meta），默认 64KB 够用，
	// 但事件里带 previous commit 时会变大，留出余量。
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // 空行与 ": keepalive" 心跳
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				URI  string `json:"uri"`
				Meta struct {
					Origin EventOrigin `json:"memcp/origin"`
				} `json:"_meta"`
			} `json:"params"`
			Error any `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			// 一帧解析不出来不该拖垮整条流：事件是加速器，
			// 丢一帧的代价只是这一次的「快」变成了等定时器。
			s.logf("op-sync: %s sent an unparsable event frame: %v", rm.Name, err)
			continue
		}
		if msg.Error != nil {
			return fmt.Errorf("subscriptions/listen returned an error: %v", msg.Error)
		}
		if msg.Method != "notifications/resources/updated" {
			// acknowledged / 结束帧（带 result）走这里。
			continue
		}
		if isSelfOrigin(msg.Params.Meta.Origin, self) {
			// 本机推过去的内容被对端广播回来：拉一轮只会是 0 条。
			continue
		}
		if fn := s.peerEventHook.Load(); fn != nil {
			(*fn)()
		}
		s.kickSync(kickPull)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}
