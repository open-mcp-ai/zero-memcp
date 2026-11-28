package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// apply 必须一次改写多个文件、并在一次往返内完成。
func TestApplyWritesAndDeletesInOneCall(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	// 先种一个「待删除」的文件和一个「待改写」的文件
	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 原始记忆"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPVars(t, srv, tok, "blackegg/me@zero:SKILL.md", "# 旧索引\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPVars(t, srv, tok, "blackegg/me@zero:08-old.md", "旧内容"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}

	patches := []map[string]string{
		{"op": "write", "path": "SKILL.md", "content": "# 新索引\n\n- [07-topic](07-topic.md)\n"},
		{"op": "write", "path": "07-topic.md", "content": "# 新主题\n\n正文。\n"},
		{"op": "delete", "path": "08-old.md"},
	}
	raw, _ := json.Marshal(patches)

	cmd := "apply blackegg/me@zero $PATCHES"
	out, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  cmd,
			"vars": map[string]string{"PATCHES": string(raw)},
		},
	})
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if !strings.Contains(out, "applied 3 change(s)") {
		t.Fatalf("输出不对: %s", out)
	}
	if !strings.Contains(out, "2 written, 1 deleted") {
		t.Fatalf("统计不对: %s", out)
	}

	// 三个结果都必须立刻可见（脏层，不需要 commit）
	got := mustCall(t, srv, tok, "read blackegg/me@zero:SKILL.md")
	if !strings.Contains(got, "新索引") {
		t.Fatalf("SKILL.md 没被改写: %s", got)
	}
	got = mustCall(t, srv, tok, "read blackegg/me@zero:07-topic.md")
	if !strings.Contains(got, "新主题") {
		t.Fatalf("新文件没写进去: %s", got)
	}
	mustMiss(t, srv, tok, "read blackegg/me@zero:08-old.md")
}

// 校验必须发生在写第一个字节之前：一批里有一条非法，整批都不落盘。
//
// 这是 apply 相比「连续调 N 次 write」的核心价值——半批改动比没有改动更难收拾。
func TestApplyRejectsWholeBatchOnBadPatch(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 原始记忆"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}

	// 第一条合法、第二条非法（路径逃出树）
	bad := `[{"op":"write","path":"good.md","content":"应该不落盘"},{"op":"write","path":"../escape.md","content":"x"}]`
	_, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "apply blackegg/me@zero $PATCHES",
			"vars": map[string]string{"PATCHES": bad},
		},
	})
	if err == nil {
		t.Fatal("非法路径应被拒绝")
	}
	if !strings.Contains(err.Error(), "inside the tree") {
		t.Fatalf("错误信息没说清是路径逃逸: %v", err)
	}

	// good.md 不该存在：整批被拒
	mustMiss(t, srv, tok, "read blackegg/me@zero:good.md")
}

// 同一路径在一次 apply 里出现两次必须被拒——后者会静默覆盖前者。
func TestApplyRejectsDuplicatePath(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 原始记忆"); err != nil {
		t.Fatal(err)
	}
	dup := `[{"op":"write","path":"a.md","content":"1"},{"op":"write","path":"a.md","content":"2"}]`
	_, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "apply blackegg/me@zero $PATCHES",
			"vars": map[string]string{"PATCHES": dup},
		},
	})
	if err == nil {
		t.Fatal("重复路径应被拒绝")
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("错误信息不对: %v", err)
	}
}
