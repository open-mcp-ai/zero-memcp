package server

import (
	"encoding/json"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// srecall 回显必须包含语言判定三件套：query_lang / query_zh_ratio / semantic_route。
//
// 为什么必须回显：同一个查询，昨天按中文搜和今天按英文搜，分数天然不同。
// 没有回显，两次结果差异就无法归因，排查变成玄学。
func TestSRecallReportsQueryLanguageAudit(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	out, err := callMCP(t, srv, tok, "srecall 排查Ollama的GPU占用 --json")
	if err != nil {
		t.Fatalf("srecall: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json parse %q: %v", out, err)
	}
	lang, ok := res["query_lang"].(string)
	if !ok || lang == "" {
		t.Fatalf("输出必须包含 query_lang，实际没有：%v", res)
	}
	if _, ok := res["query_zh_ratio"].(float64); !ok {
		t.Fatalf("输出必须包含 query_zh_ratio，实际没有：%v", res)
	}
	if _, ok := res["semantic_route"].(string); !ok {
		t.Fatalf("输出必须包含 semantic_route，实际没有：%v", res)
	}
	t.Logf("语言判定回显通过: query=%q lang=%s zh_ratio=%.2f route=%s",
		res["query"], lang, res["query_zh_ratio"], res["semantic_route"])
}
