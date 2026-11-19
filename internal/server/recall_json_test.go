package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// recall --json 必须输出合法的 JSON 字符串，且同时包含 hits 与 results 两个键。
//
// 理由：唤醒前闸门（zeroncall 的 wake gate）需要直接按点路径取分数：
//
//	"pick": "results[0].score"
//
// 如果只有自然语言排版（"addr (73.79)"），程序消费者就必须写正则去抠，
// 而正则对排版变化毫无抵抗力——加个空格或改个小数位就静默取不到分。
func TestRecallJSONFlagReturnsStructuredText(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	// 先种两篇可被搜到的记忆
	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 密码学与身份认证探讨"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}

	// 1. 命中时：必须是一行合法的 JSON，content.text 里直接是 JSON 字符串
	out, err := callMCP(t, srv, tok, "recall 密码学 --json")
	if err != nil {
		t.Fatalf("recall --json 失败: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("输出不是以 { 开头的 JSON: %s", out)
	}

	var d struct {
		Hits []struct {
			Addr  string  `json:"addr"`
			Score float64 `json:"score"`
		} `json:"hits"`
		Results []struct {
			Addr  string  `json:"addr"`
			Score float64 `json:"score"`
		} `json:"results"`
		Count  int    `json:"count"`
		Query  string `json:"query"`
		TookMs *int64 `json:"tookMs"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("解析 recall --json 失败: %v，原文: %s", err, out)
	}
	if d.Count != 1 || len(d.Hits) != 1 || len(d.Results) != 1 {
		t.Fatalf("命中数不对: count=%d hits=%d results=%d", d.Count, len(d.Hits), len(d.Results))
	}
	if d.Hits[0].Score <= 0 {
		t.Fatalf("分数未正常输出: score=%f", d.Hits[0].Score)
	}
	// hits 与 results 必须完全同构（别名）
	if d.Hits[0].Score != d.Results[0].Score || d.Hits[0].Addr != d.Results[0].Addr {
		t.Fatalf("hits 与 results 别名不同步: hits=%+v results=%+v", d.Hits[0], d.Results[0])
	}

	// 审计字段：query 必须原样回显（不是改写后的查询词），tookMs 必须存在。
	//
	// 调用方靠这两个字段把「哪条消息、多少分、放行还是拒绝」串成一条链。
	// 不回显查询，它对账时无法区分「这条消息没查」和「查了但查询词被改了」。
	if d.Query != "密码学" {
		t.Fatalf("query 没有原样回显: %q", d.Query)
	}
	if d.TookMs == nil {
		t.Fatalf("缺少 tookMs 字段")
	}
	if *d.TookMs < 0 {
		t.Fatalf("tookMs 不应为负: %d", *d.TookMs)
	}

	// 2. 空结果时：也必须是合法 JSON，而不是 "no matches"
	emptyOut, err := callMCP(t, srv, tok, "recall 毫无关系的火星文xyz123 --json")
	if err != nil {
		t.Fatalf("空结果 recall --json 报错: %v", err)
	}
	var emptyD struct {
		Hits    []any  `json:"hits"`
		Results []any  `json:"results"`
		Count   int    `json:"count"`
		Query   string `json:"query"`
		TookMs  *int64 `json:"tookMs"`
	}
	if err := json.Unmarshal([]byte(emptyOut), &emptyD); err != nil {
		t.Fatalf("空结果不是合法 JSON: %v, 原文: %s", err, emptyOut)
	}
	if emptyD.Count != 0 || len(emptyD.Hits) != 0 || len(emptyD.Results) != 0 {
		t.Fatalf("空结果应该 count=0: %+v", emptyD)
	}
	if emptyD.Query == "" || emptyD.TookMs == nil {
		t.Fatalf("空结果同样要带 query/tookMs: %+v", emptyD)
	}

	// 3. 不带 --json 时保持原有人话排版（向前兼容）
	legacyOut, err := callMCP(t, srv, tok, "recall 密码学")
	if err != nil {
		t.Fatalf("原版 recall 失败: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(legacyOut), "{") {
		t.Fatalf("不带 --json 时不该返回 JSON: %s", legacyOut)
	}
}
