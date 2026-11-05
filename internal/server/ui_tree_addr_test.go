package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// TestBuildTreeFromBlocksEmitsAddr 锁定两件事：
//
//  1. 文件节点必须带 addr 字段。前端 renderNode 绑定
//     onclick="openFile('${n.addr}','${treePath}')"；
//     只给 name/size 时 n.addr 是 undefined，请求变成
//     /api?action=read&addr=undefined，后端报 "got undefined"。
//
//  2. addr 必须是 <project>@<branch>:<path> 的完整形式。
//     早先只拼树内路径，前端点开文件时缺了项目与分支，
//     resolveAddr 只能靠「相对路径在所有仓库里找唯一命中」的
//     兜底逻辑——多个项目有同名文件时直接报歧义。
func TestBuildTreeFromBlocksEmitsAddr(t *testing.T) {
	blocks := []memory.Block{
		{Project: "weft", Branch: "main.record", Path: "record/2026-11-04/sample.md", Size: 123},
		{Project: "weft", Branch: "main.record", Path: "thought/nested/deep/t.md", Size: 456},
	}
	tree := buildTreeFromBlocks("weft@main.record", "weft@main.record", blocks)
	if tree["name"] != "weft@main.record" || tree["type"] != "dir" {
		t.Fatalf("root unexpected: %+v", tree)
	}

	var addrs []string
	var walk func(map[string]any)
	walk = func(n map[string]any) {
		if n["type"] == "file" {
			addr, _ := n["addr"].(string)
			if addr == "" {
				t.Errorf("file node missing addr: %+v", n)
				return
			}
			addrs = append(addrs, addr)
			return
		}
		children, _ := n["children"].([]map[string]any)
		for _, c := range children {
			walk(c)
		}
	}
	walk(tree)

	if len(addrs) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(addrs), addrs)
	}
	want := map[string]bool{
		"weft@main.record:record/2026-11-04/sample.md": true,
		"weft@main.record:thought/nested/deep/t.md":    true,
	}
	for _, a := range addrs {
		if !want[a] {
			t.Errorf("unexpected addr %q; want one of %v", a, keysOf(want))
		}
		if !strings.Contains(a, "@") || !strings.Contains(a, ":") {
			t.Errorf("addr %q is not <project>@<branch>:<path>", a)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
