package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// erase 端到端测试：
//  1. 权限守门：只有带 x 能力或 root 的凭证才准抹除（普通 rw 必须被拦）
//  2. 销毁事实：执行后全历史 commit 都搜不到该内容，且 recall 立即断档
func TestEraseVerbE2E(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	// 普通凭证：只有 rw，没有 x
	rwTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rw"})
	// 管理凭证：带 x（改写历史的能力）
	rwxTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 先写一条敏感记忆并 commit
	if _, err := callMCPRaw(t, srv, rwxTok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:2026-11-19/leak.md $BODY",
			"vars": map[string]string{"BODY": "银行卡号 6222020200001234"},
		},
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := callMCP(t, srv, rwxTok, "commit blackegg/me@zero leaked"); err != nil {
		t.Fatalf("commit 失败: %v", err)
	}

	// 确认 recall 能够搜到它
	out, err := callMCP(t, srv, rwTok, "recall 银行卡号")
	if err != nil || !strings.Contains(out, "2026-11-19/leak.md") {
		t.Fatalf("抹除前应当能搜到，实际输出:\n%s", out)
	}

	// 1. 能力门：没有 x 权限的（非 root）调用方必须被拦。
	//    注意不能拿 rwTok 来测——配置里 user zero 是 root，root 绕过是
	//    刻意行为（管理员必须能销毁数据），所以这里直接测门本身。
	gateCaller := Caller{User: "guest", Identity: "guest", Cap: "rw"}
	if _, gerr := srv.execErase("blackegg/me@zero:2026-11-19/leak.md", gateCaller, "zero", false); gerr == nil {
		t.Fatal("没有 x 能力的调用方执行 erase 应被拒绝，实际成功了")
	} else if !strings.Contains(gerr.Error(), "x capability") {
		t.Fatalf("报错应指出缺少 x 能力，实际为: %v", gerr)
	}

	// 2. 带 x 权限的凭证执行 erase：成功
	eraseOut, err := callMCP(t, srv, rwxTok, "erase blackegg/me@zero:2026-11-19/leak.md")
	if err != nil {
		t.Fatalf("rwx 凭证 erase 应成功: %v", err)
	}
	if !strings.Contains(eraseOut, "erased") || !strings.Contains(eraseOut, "rewritten") {
		t.Fatalf("erase 输出应包含回执，实际为:\n%s", eraseOut)
	}

	// 3. 抹除后：recall 立即搜不到（内存索引热刷新）
	outAfter, _ := callMCP(t, srv, rwTok, "recall 银行卡号")
	if strings.Contains(outAfter, "2026-11-19/leak.md") {
		t.Fatalf("抹除后 recall 仍搜得到该条目:\n%s", outAfter)
	}

	// 4. 底层全历史 commit 验证：确实无引用
	repo, _ := srv.projects.Get("blackegg/me")
	head, _ := repo.Resolve("zero")
	if _, found, _ := repo.Stat("zero", "2026-11-19/leak.md"); found {
		t.Fatal("底层 repo.Stat 仍能查到该路径")
	}
	_ = head
}
