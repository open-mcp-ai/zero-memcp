package zeromem

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalConfig 是一份「一个 root 用户 + 一个可写项目」的最小配置。
//
// 没有它，写入会因为「没有已声明项目」而拒绝——这是设计上正确的行为
// （不猜落点），所以测试必须把配置也一起准备好。
const minimalConfig = `
[server]
data     = "%s"
endpoint = "test"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`

// newCore 造一个最小可用的记忆引擎。
func newCore(t *testing.T, mutate func(*Options)) *Core {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "zero-memcp.toml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(minimalConfig, dataDir)), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	opts := Options{
		DataDir:        dataDir,
		ConfigFile:     cfgPath,
		Endpoint:       "test",
		DefaultProfile: "zero",
	}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestNewRequiresDataDir(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("没有 DataDir 时应报错")
	}
}

func TestToolDefShape(t *testing.T) {
	c := newCore(t, nil)
	def, fn := c.Tool()
	if fn == nil {
		t.Fatal("Tool 必须返回可调用函数")
	}
	// 对外承诺只有一个工具，且名字固定为 mem。
	if def.Name != "mem" {
		t.Fatalf("工具名应为 mem，得到 %q", def.Name)
	}
	if def.Description == "" {
		t.Fatal("工具描述不能为空：模型靠它决定敲哪一行")
	}
	props, ok := def.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema 缺 properties")
	}
	for _, want := range []string{"cmd", "args", "vars"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("schema 缺 %q", want)
		}
	}
	req, ok := def.InputSchema["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "cmd" {
		t.Fatalf("required 应只有 cmd，得到 %v", def.InputSchema["required"])
	}
	if defs := c.ToolDefs(); len(defs) != 1 || defs[0].Name != "mem" {
		t.Fatalf("ToolDefs 应只含 mem，得到 %+v", defs)
	}
}

// 正文经 vars 传入，必须原样落盘：这条链路是「指令与数据分离」的落地点。
func TestPutThenRecall(t *testing.T) {
	c := newCore(t, nil)
	_, call := c.Tool()
	ctx := context.Background()

	body := "记忆体的一句话：先落盘，再确认。含 -- 也不该被截断。"
	args, err := json.Marshal(map[string]any{
		"cmd":  "put -- $TEXT",
		"vars": map[string]string{"TEXT": body},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(ctx, args)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if strings.Contains(out, "语法") {
		t.Fatalf("put 被当成语法错误: %s", out)
	}

	hits, err := call(ctx, json.RawMessage(`{"cmd":"recall 落盘"}`))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if strings.Contains(hits, "无命中") {
		t.Fatalf("刚写入的内容 recall 不到：%s", hits)
	}
}

// 未被引用的变量必须报错：否则注入面就从「没有拼接」变成「静默忽略」。
func TestUnusedVarRejected(t *testing.T) {
	c := newCore(t, nil)
	_, call := c.Tool()
	args := json.RawMessage(`{"cmd":"status","vars":{"NOPE":"x"}}`)
	if _, err := call(context.Background(), args); err == nil {
		t.Fatal("cmd 没有引用 vars 时应报错")
	}
}

func TestUnknownVerbSuggests(t *testing.T) {
	c := newCore(t, nil)
	_, call := c.Tool()
	out, err := call(context.Background(), json.RawMessage(`{"cmd":"putt -- x"}`))
	if err == nil && !strings.Contains(out, "没有") {
		t.Fatalf("未知动词应报错，得到 %q", out)
	}
	if err != nil && !strings.Contains(err.Error(), "put") {
		t.Fatalf("未知动词的错误里应给出建议，得到 %v", err)
	}
}

// Mux 与 ReposHandler 必须能独立装配，且 /health 可探测。
func TestMuxAndReposHandler(t *testing.T) {
	c := newCore(t, nil)

	rec := httptest.NewRecorder()
	c.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health 应 200，得到 %d（body=%s）", rec.Code, rec.Body.String())
	}

	// /repos/ 在默认端口上（无 listen 配置 → allow=*）应对未带凭证的请求
	// 给出 401/403，而不是 404：端点存在但需要授权。
	rec = httptest.NewRecorder()
	c.ReposHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/x.git/info/refs?service=git-upload-pack", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("ReposHandler 不该 404（端点应已注册），body=%s", rec.Body.String())
	}
}
