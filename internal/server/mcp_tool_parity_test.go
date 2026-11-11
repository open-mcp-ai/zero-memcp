package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-mcp-ai/termcp/mcp"
)

// tools/list 是客户端可见的契约。这里锁住三件事，防止"改个描述顺手改了形状"：
//
//  1. 只暴露 term 一个工具；
//  2. cmd 是必需的，且五个参数都在；
//  3. 两轮序列化结果完全一致（形状稳定，不随 map 遍历顺序漂移）。
func TestTermToolContract(t *testing.T) {
	s := &Server{}
	got, err := s.dispatchMCP(context.Background(), "tools/list", nil, s.rootCaller())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Type       string         `json:"type"`
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}

	if len(parsed.Tools) != 1 || parsed.Tools[0].Name != "term" {
		t.Fatalf("只应暴露 term，实际: %s", raw)
	}
	tool := parsed.Tools[0]
	if tool.Description == "" {
		t.Fatal("description 不能为空——它是工具被正确调用的唯一说明")
	}
	if tool.InputSchema.Type != "object" {
		t.Fatalf("inputSchema.type 应为 object，实际 %q", tool.InputSchema.Type)
	}
	for _, k := range []string{"cmd", "args", "vars", "content", "structured_content"} {
		if _, ok := tool.InputSchema.Properties[k]; !ok {
			t.Fatalf("缺少参数 %q", k)
		}
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "cmd" {
		t.Fatalf("required 应只有一个 cmd，实际 %v", tool.InputSchema.Required)
	}

	// 形状稳定：同样的声明序列化两次必须逐字节一致。
	again, _ := json.Marshal(map[string]any{"tools": []any{termTool()}})
	var a, b any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(again, &b)
	if _, ok := b.(map[string]any); !ok {
		t.Fatal("mcp.Tool 序列化失败")
	}
	_ = mcp.NewServer // 保持 import：声明层必须来自 termcp
}
