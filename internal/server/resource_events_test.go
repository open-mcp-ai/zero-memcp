package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// listenProbe 是一个简化的事件流客户端，只做到测试所需的程度。
type listenProbe struct {
	resp   *http.Response
	reader *bufio.Reader
}

func openListen(t *testing.T, srv *Server, addr, session string, uris ...string) *listenProbe {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "subscriptions/listen",
		"params": map[string]any{
			"notifications": map[string]any{"resourceSubscriptions": uris},
		},
	})
	req, err := http.NewRequest(http.MethodPost, addr+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Memcp-Session", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return &listenProbe{resp: resp, reader: bufio.NewReader(resp.Body)}
}

// nextFrame 读一帧 SSE，返回 JSON-RPC 消息体。
func (p *listenProbe) nextFrame(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	type res struct {
		msg map[string]any
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var data string
		for {
			line, err := p.reader.ReadString('\n')
			if err != nil {
				ch <- res{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if data == "" {
					continue
				}
				var m map[string]any
				ch <- res{msg: m, err: json.Unmarshal([]byte(data), &m)}
				return
			}
			if strings.HasPrefix(line, "data: ") {
				data += strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("读帧失败: %v", r.err)
		}
		return r.msg
	case <-time.After(timeout):
		t.Fatal("等帧超时")
		return nil
	}
}

// startListenServer 起一个真实监听的服务，返回地址。
func startListenServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"
endpoint = "testnode"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`
	cfgP := filepath.Join(dataDir, "zero-memcp.toml")
	if err := osWrite(cfgP, cfg); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		DataDir:    dataDir,
		ConfigFile: cfgP,
		Listen:     "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go func() { _ = srv.Start() }()

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "seed.md", []byte("seed"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}

	addr := "http://" + srv.Addr()
	// 等监听就绪
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(addr + "/health"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return srv, addr, dataDir
}

// TestListenHandshakeAndEvent 锁定订阅闭环：
// listen → ack → 写入+提交 → resources/updated（带 origin）。
func TestListenHandshakeAndEvent(t *testing.T) {
	srv, addr, _ := startListenServer(t)

	probe := openListen(t, srv, addr, "sess-A", "memcp://daily/zero")
	defer probe.resp.Body.Close()

	// 1. ack 必须是第一帧
	ack := probe.nextFrame(t, 3*time.Second)
	if got := ack["method"]; got != "notifications/subscriptions/acknowledged" {
		t.Fatalf("第一帧应是 acknowledged，实际 %v", got)
	}

	// 2. 写入并提交
	if _, err := srv.execTerm("put daily@zero/team -- hello", Viewer{}, srv.rootCaller()); err != nil {
		t.Fatal(err)
	}
	repo, _ := srv.projects.Get("daily")
	if _, err := repo.Commit("zero", "test"); err != nil {
		t.Fatal(err)
	}
	prev, _ := repo.Resolve("zero")
	// 再写一次产生真正的前进
	if _, err := srv.execTerm("put daily@zero/team -- hello2", Viewer{}, srv.rootCaller()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "test2"); err != nil {
		t.Fatal(err)
	}
	// 用服务自身的提交路径产生事件（带 origin）
	c := srv.rootCaller()
	c.Session = "sess-A"
	head, _ := repo.Resolve("zero")
	srv.publishTreeChange("daily", "zero", "", prev, head, EventOrigin{
		Identity: c.Identity, Session: c.Session, Endpoint: srv.cfg.Endpoint,
	})

	// 3. 收到 resources/updated
	ev := probe.nextFrame(t, 3*time.Second)
	if got := ev["method"]; got != "notifications/resources/updated" {
		t.Fatalf("应收到 resources/updated，实际 %v", got)
	}
	params, _ := ev["params"].(map[string]any)
	if params["uri"] != "memcp://daily/zero" {
		t.Fatalf("uri 不符: %v", params["uri"])
	}
	meta, _ := params["_meta"].(map[string]any)
	origin, _ := meta["memcp/origin"].(map[string]any)
	if origin["session"] != "sess-A" {
		t.Fatalf("origin.session 应为 sess-A，实际 %v", origin["session"])
	}
	if origin["identity"] == "" {
		t.Fatal("origin.identity 不该为空")
	}
}

// TestListenRejectsOutOfScope 锁定订阅时的权限校验：
// 越权订阅必须在握手阶段就失败，而不是订阅成功但永远收不到事件。
func TestListenRejectsOutOfScope(t *testing.T) {
	_, addr, dataDir := startListenServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "outsider", Cap: "rwx",
		// 只挂 daily：订阅别的项目必须被拒。
		Mount: []auth.MountPoint{{Project: "daily", At: "/", Cap: "rwx"}},
	})

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "subscriptions/listen",
		"params": map[string]any{
			"notifications": map[string]any{
				"resourceSubscriptions": []string{"memcp://secret/zero"},
			},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, addr+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("越权订阅应 403，实际 %d", resp.StatusCode)
	}
}

// TestSubscriptionPrefixMatch 锁定前缀匹配：订阅分支则子树变更也算。
func TestSubscriptionPrefixMatch(t *testing.T) {
	uri := treeURI("daily", "zero", "")
	if !subMatches([]string{uri}, treeURI("daily", "zero", "team/chat/01.md")) {
		t.Fatal("订阅分支应覆盖子路径")
	}
	if subMatches([]string{treeURI("daily", "zero", "team")}, treeURI("daily", "zero", "other/x.md")) {
		t.Fatal("订阅子树不该覆盖兄弟路径")
	}
	if !subMatches([]string{treeURI("daily", "zero", "")}, uri) {
		t.Fatal("精确匹配应成立")
	}
}

// TestInitializeDeclaresModernProtocol 锁定版本与能力声明不再是假的。
func TestInitializeDeclaresModernProtocol(t *testing.T) {
	srv, _, _ := startListenServer(t)
	res, err := srv.dispatchMCP(t.Context(), "initialize", json.RawMessage(`{}`), srv.rootCaller())
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["protocolVersion"] != "2026-07-28" {
		t.Fatalf("协议版本应为 2026-07-28（subscriptions/listen 所在修订版），实际 %v", m["protocolVersion"])
	}
	caps, _ := m["capabilities"].(map[string]any)
	if _, ok := caps["resources"]; !ok {
		t.Fatal("必须声明 resources 能力")
	}
	info, _ := m["serverInfo"].(map[string]any)
	if _, bad := info["transport"]; bad {
		t.Fatal("serverInfo.transport 不该再出现（早先它谎称 streamable-http）")
	}
}


// TestTreeURIRoundTripWithSlashInProject 锁定项目名含斜杠时的 URI 编解码。
//
// 这个 bug 极其难发现：treeURI 与 parseTreeURI 用同一套逻辑，两边错得
// 一样，字符串恰好相等，端到端测试会「通过」。只有换成真实凭证才暴露
// （root 全放行掩盖了越界，受限凭证直接 403）。
func TestTreeURIRoundTripWithSlashInProject(t *testing.T) {
	cases := []struct{ project, branch, path string }{
		{"BlackEggAI/zero", "main.record", ""},
		{"BlackEggAI/zero", "main.record", "thought/2026-11-06"},
		{"daily", "zero", "team/chat/01.md"},
		{"a/b/c", "br", ""},
	}
	for _, c := range cases {
		uri := treeURI(c.project, c.branch, c.path)
		p, b, path, err := parseTreeURI(uri)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", uri, err)
		}
		if p != c.project || b != c.branch || path != c.path {
			t.Fatalf("往返不一致: %q → project=%q branch=%q path=%q，期望 %q/%q/%q",
				uri, p, b, path, c.project, c.branch, c.path)
		}
	}
}

// TestTreeURIEncodesSlash 锁定编码形态：项目名里的斜杠必须进 percent-encoding，
// 否则路径分隔符就是歧义的。
func TestTreeURIEncodesSlash(t *testing.T) {
	uri := treeURI("BlackEggAI/zero", "main.record", "")
	if strings.Contains(strings.TrimPrefix(uri, "memcp://"), "BlackEggAI/zero") {
		t.Fatalf("项目名里的斜杠没有转义，URI 会错位: %q", uri)
	}
	if uri != "memcp://BlackEggAI%2Fzero/main.record" {
		t.Fatalf("URI 形态不符: %q", uri)
	}
	// 解析回来必须是完整的项目名
	p, b, _, err := parseTreeURI(uri)
	if err != nil || p != "BlackEggAI/zero" || b != "main.record" {
		t.Fatalf("解出 project=%q branch=%q err=%v", p, b, err)
	}
}

// TestSubscriptionMatchesRealProjectURI 锁定前缀匹配在真实项目名下成立。
func TestSubscriptionMatchesRealProjectURI(t *testing.T) {
	branchURI := treeURI("BlackEggAI/zero", "main.record", "")
	subURI := treeURI("BlackEggAI/zero", "main.record", "thought")
	if !subMatches([]string{branchURI}, subURI) {
		t.Fatal("订阅分支应覆盖其子树（真实项目名含斜杠）")
	}
	if !subMatches([]string{treeURI("BlackEggAI/zero", "main.record", "")}, branchURI) {
		t.Fatal("精确匹配应成立")
	}
}
