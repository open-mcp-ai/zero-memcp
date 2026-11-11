package server

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// tools/list 的输出是客户端可见的契约：字段名、required 的位置、default 的写法。
// 迁移到 mcp.Tool 之前先证明两条路径产出的是同一份东西——不一致就以现有实现为准，
// 而不是让客户端替我们发现差异。
func TestTermToolSchemaParity(t *testing.T) {
	s := &Server{}
	got, err := s.dispatchMCP(context.Background(), "tools/list", nil, s.rootCaller())
	if err != nil {
		t.Fatal(err)
	}

	legacy, _ := json.Marshal(got)
	newShape, _ := json.Marshal(map[string]any{"tools": []any{termTool()}})

	var a, b any
	if err := json.Unmarshal(legacy, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(newShape, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		prettyA, _ := json.MarshalIndent(a, "", "  ")
		prettyB, _ := json.MarshalIndent(b, "", "  ")
		t.Fatalf("两条路径的 tools/list 不一致\n--- legacy ---\n%s\n--- mcp.Tool ---\n%s", prettyA, prettyB)
	}
}
