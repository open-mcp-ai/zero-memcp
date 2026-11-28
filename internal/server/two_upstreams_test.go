package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 同一条分支挂两个上游会发生什么？
//
// 这是「两个上游 mem 能不能配」的真问题：结构上配置支持多个 [[remote]]，
// 水位也按 (project, branch, peer) 分开记；但**权威整树（Replace）**
// 在语义上只能有一个来源——它宣称「不在批次里的都是已删除」。
// 两个上游各发一棵树，就会互相把对方的文件判成删除。
//
// 这个测试把现状量出来，作为「要不要加权威声明」的依据。
func TestTwoUpstreamsOnSameBranch(t *testing.T) {
	// 上游 A：a.md + shared.md
	upA, upAData := setupMountServer(t)
	tokA := mintRestricted(t, upAData, auth.Claims{Sub: "zero", Cap: "rwx"})
	writeFile := func(s *Server, tok, path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, s, tok, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + path + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	writeFile(upA, tokA, "a.md", "来自 A 的 a")
	writeFile(upA, tokA, "shared.md", "A 的版本")

	// 上游 B：b.md + shared.md（内容不同）
	upB, upBData := setupMountServer(t)
	tokB := mintRestricted(t, upBData, auth.Claims{Sub: "zero", Cap: "rwx"})
	writeFile(upB, tokB, "b.md", "来自 B 的 b")
	writeFile(upB, tokB, "shared.md", "B 的版本")

	srvA := httptest.NewServer(upA.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srvA.Close()
	srvB := httptest.NewServer(upB.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srvB.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	down, downData := setupMountServer(t)
	tokDown := mintRestricted(t, downData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 先拉 A。
	if _, err := down.pullOpsFromPeer(client, srvA.URL, tokA, "upA", "blackegg/me", "zero"); err != nil {
		t.Fatalf("拉 A: %v", err)
	}
	if out, err := callMCP(t, down, tokDown, "read blackegg/me@zero:a.md"); err != nil || out == "" {
		t.Fatalf("拉 A 之后 a.md 应可读: %v", err)
	}

	// 再拉 B。B 的权威树里没有 a.md。
	if _, err := down.pullOpsFromPeer(client, srvB.URL, tokB, "upB", "blackegg/me", "zero"); err != nil {
		t.Fatalf("拉 B: %v", err)
	}

	// 量三件事：A 的独有文件是否被 B 判成删除；B 的文件是否到了；shared.md 归谁。
	_, errA := callMCP(t, down, tokDown, "read blackegg/me@zero:a.md")
	_, errB := callMCP(t, down, tokDown, "read blackegg/me@zero:b.md")
	sharedOut, errShared := callMCP(t, down, tokDown, "read blackegg/me@zero:shared.md")

	t.Logf("拉完两个上游之后：")
	t.Logf("  a.md    : err=%v（nil 表示仍在）", errA)
	t.Logf("  b.md    : err=%v（nil 表示已在）", errB)
	t.Logf("  shared.md: err=%v content=%q", errShared, firstLine(sharedOut))

	// 记录现状：如果 a.md 被删，说明两个上游的权威树互相清场。
	if errA != nil {
		t.Logf("→ 结论：A 的独有文件被 B 的权威整树清掉了（Replace 语义冲突）")
	} else {
		t.Logf("→ 结论：A 的独有文件仍在（说明 B 的整树没有生效为 Replace）")
	}
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

var _ = fmt.Sprintf
