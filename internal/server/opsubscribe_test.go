package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// 订阅 URI 必须与真正会拉的分支面一致，且项目名要转义。
//
// 项目名本身含斜杠（"BlackEggAI/zero"），不转义会被解析成
// project="BlackEggAI", branch="zero"——全错位。这个 bug 在生成与解析
// 用同一套错误逻辑时端到端「通过」，所以必须在这里钉死字面量。
func TestPeerSubscribeURIs(t *testing.T) {
	rm := config.Remote{
		Name:     "hub",
		Pull:     []string{"main", "main.record", "main"},
		Push:     []string{"ignored-when-pull-set"},
		AutoSync: true,
	}
	got := peerSubscribeURIs("BlackEggAI/zero", rm)
	want := []string{
		"memcp://BlackEggAI%2Fzero/main",
		"memcp://BlackEggAI%2Fzero/main.record",
	}
	if len(got) != len(want) {
		t.Fatalf("期望 %d 条（去重后），实际 %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条: 期望 %q，实际 %q", i, want[i], got[i])
		}
	}

	// Pull 为空回落到 Push：订阅面必须与真正的拉取面一致，
	// 否则会出现「订阅了但不拉」或「拉了但没订阅」。
	rm2 := config.Remote{Push: []string{"main"}, AutoSync: true}
	if got := peerSubscribeURIs("BlackEggAI/zero", rm2); len(got) != 1 || got[0] != "memcp://BlackEggAI%2Fzero/main" {
		t.Fatalf("Pull 为空应回落到 Push，实际 %v", got)
	}
}

// 「本机推过去的内容被对端广播回来」必须能认出来，否则每一轮推送
// 都会换来一次空转的拉取。
//
// 判据是 Identity 里的 "peer:<本机端点名>"，不是 Endpoint——对端落地
// 本机推送时 Endpoint 填的是**它自己**。写成 Endpoint 的话永远不会相等，
// 一次也不会生效，而且不报错。
func TestIsSelfOrigin(t *testing.T) {
	self := "laptop"

	// 对端落地本机推送时的真实形状（见 opapply.go / syncwork.go）。
	fromPeerLanding := EventOrigin{Identity: "peer:laptop", Endpoint: "3060"}
	if !isSelfOrigin(fromPeerLanding, self) {
		t.Fatal("对端落地本机推送必须认成自己——否则每轮推送换来一次空转拉取")
	}

	// 对端自己写的内容：本机应当被叫醒。
	peerOwnWrite := EventOrigin{Identity: "zero", Session: "s1", Endpoint: "3060"}
	if isSelfOrigin(peerOwnWrite, self) {
		t.Fatal("对端自己写的必须叫醒本机，不能跳过")
	}

	// 外部推进（另一台机器 git push）：也要叫醒。
	external := EventOrigin{Identity: "external", Endpoint: "3060"}
	if isSelfOrigin(external, self) {
		t.Fatal("外部推进必须叫醒本机")
	}

	// 判不出来时一律不跳过：少 woke 一次只是慢一点，误跳过是真漏。
	if isSelfOrigin(EventOrigin{}, "laptop") {
		t.Fatal("origin 缺失时不得跳过")
	}
	if isSelfOrigin(fromPeerLanding, "") {
		t.Fatal("本机端点名为空时不得跳过")
	}
}

// shouldSubscribe 的默认判据：工作层对端 + auto_sync + 会拉内容。
func TestShouldSubscribeDefaults(t *testing.T) {
	base := config.Remote{
		Name: "hub", URL: "http://10.0.0.1:1729",
		AutoSync: true, Push: []string{"main"},
	}
	if !shouldSubscribe(base) {
		t.Fatal("工作层对端 + auto_sync + 有分支列表，默认应当订阅")
	}

	backup := base
	backup.PushOnly = true
	if shouldSubscribe(backup) {
		t.Fatal("push_only 的备份节点不该订阅：我们从不从它拉")
	}

	noAuto := base
	noAuto.AutoSync = false
	if shouldSubscribe(noAuto) {
		t.Fatal("没开 auto_sync 时订阅没有意义（没有跑同步的循环）")
	}

	gitRemote := base
	gitRemote.URL = "https://github.com/x/y.git"
	if shouldSubscribe(gitRemote) {
		t.Fatal("Git 远端没有 MCP 端点，不该订阅")
	}

	off := false
	explicitOff := base
	explicitOff.Subscribe = &off
	if shouldSubscribe(explicitOff) {
		t.Fatal("显式 subscribe = false 必须生效")
	}

	on := true
	explicitOn := gitRemote
	explicitOn.Subscribe = &on
	if !shouldSubscribe(explicitOn) {
		t.Fatal("显式 subscribe = true 应当优先于默认判据")
	}
}

// 端到端：A 订阅 B 的分支；B 写入并广播；A 必须被叫醒。
// 再在 B 广播一条「来自 A 自己推送」的事件：A 必须跳过，不能空转。
func TestPeerSubscriptionWakesOnPeerChangeAndSkipsSelf(t *testing.T) {
	peerSrv, peerData := setupMountServer(t)
	peerHTTP := httptest.NewServer(peerSrv.muxFor(ListenBlock{Addr: "127.0.0.1:0", Allow: []string{"*"}}))
	peerURL := peerHTTP.URL
	peerTok := mintRestricted(t, peerData, auth.Claims{Sub: "zero", Cap: "rwx"})

	self, _ := setupMountServer(t)
	self.cfg.Endpoint = "laptop"

	// 清理顺序必须是：先关本机服务（它持有那条 SSE），再关对端。
	//
	// 反过来的话 httptest.Server.Close 会等活跃连接，而那条连接正在
	// 被本机的订阅协程接着——失败的测试就会挂到超时（实测 60s），
	// 在 CI 上直接伪装成 flake。失败必须快。
	t.Cleanup(func() {
		_ = self.Close()
		peerHTTP.CloseClientConnections()
		peerHTTP.Close()
	})

	// 必须提供持久化的凭据名——streamPeerEvents 走 optionalSecret 读它，
	// 写成 inline token 字符串会被 Reject。这里直接用空 token 吗？不，
	// peerSrv 开着 EnforceAuth=true，所以按正常路径写到 system/secrets/。
	secDir := filepath.Join(self.cfg.DataDir, "system", "secrets")
	if err := os.MkdirAll(secDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secDir, "peer-tok"), []byte(peerTok), 0o600); err != nil {
		t.Fatal(err)
	}

	rm := config.Remote{
		Project: "blackegg/me",
		Name:    "peer", URL: peerURL, AutoSync: true,
		Push: []string{"zero"}, Pull: []string{"zero"},
		HTTPToken: "peer-tok",
	}
	uris := peerSubscribeURIs("blackegg/me", rm)

	var kicks atomic.Int64
	hook := func() { kicks.Add(1) }
	self.peerEventHook.Store(&hook)
	self.syncState.runner.start()

	done := make(chan error, 1)
	go func() {
		done <- self.streamPeerEvents(rm, uris)
	}()

	// 等到对端确实登记了这条订阅再触发事件。
	waitForActiveSub(t, peerSrv.resources, 5*time.Second)

	// ① 先播一条「本机推过去落地后被对端回播」的事件——必须跳过，不加 kick。
	peerSrv.publishTreeChange("blackegg/me", "zero", "echo.md", "", "",
		EventOrigin{Identity: "peer:laptop", Endpoint: "3060"})
	// 等一小段看它会不会误触发（同时用紧随其后的真实事件收尾，避免死等）。
	time.Sleep(100 * time.Millisecond)
	if got := kicks.Load(); got != 0 {
		t.Fatalf("自己推过去被回播的事件不能叫醒本机（否则每轮多跑一倍），实际 kick=%d", got)
	}

	// ② 再在对端走真实 MCP 写入（put 与 write 两种路径）→ 都必须叫醒订阅方。
	if _, err := callMCPRaw(t, peerSrv, peerTok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "put blackegg/me@zero $BODY",
			"vars": map[string]string{"BODY": "对端写的内容"},
		},
	}); err != nil {
		t.Fatalf("对端 put 写入失败: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if kicks.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	beforeWrite := kicks.Load()
	if beforeWrite == 0 {
		t.Fatal("对端 put 变更却没叫醒订阅方——这一跳没接上，同步会退回轮询")
	}

	// write 走的是另一条写路径（writeverb.go），同样必须广播，否则
	// 规则库写入对订阅者与下游完全静默。
	if _, err := callMCPRaw(t, peerSrv, peerTok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:rule-from-peer.md $BODY",
			"vars": map[string]string{"BODY": "对端写的规则"},
		},
	}); err != nil {
		t.Fatalf("对端 write 写入失败: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if kicks.Load() > beforeWrite {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if kicks.Load() <= beforeWrite {
		t.Fatal("对端 write 变更没叫醒订阅方——writeverb.go 漏了广播")
	}

	// ③ 流已经被 ② 的断言证明在跑；服务关闭时它必须立刻退出。
	_ = self.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("服务关闭后订阅流未在限期内退出")
	}
}

// 对端明确拒绝（4xx）时必须停止重试并点名，而不是无限重连刷日志。
func TestPeerSubscriptionStopsOnRejection(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such method", http.StatusNotFound)
	}))
	defer rejecting.Close()

	self, _ := setupMountServer(t)
	rm := config.Remote{Name: "old-peer", URL: rejecting.URL, AutoSync: true, Pull: []string{"main"}}

	err := self.streamPeerEvents(rm, []string{"memcp://a%2Fb/main"})
	if err == nil {
		t.Fatal("404 必须作为错误返回，不能当成正常结束")
	}
	var unsupported *peerSubUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("4xx 应归类为 peerSubUnsupported（停止重试），实际 %T: %v", err, err)
	}
	if !strings.Contains(unsupported.Error(), "old-peer") {
		t.Fatalf("错误信息必须点名是哪个对端，实际: %v", unsupported)
	}
}

func waitForActiveSub(t *testing.T, hub *resourceHub, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		n := 0
		hub.subs.Range(func(_, _ any) bool {
			n++
			return false
		})
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("对端始终没有收到订阅请求")
}
