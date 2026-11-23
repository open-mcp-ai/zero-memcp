package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/index"
)

// 索引必须配置才生效。没有配置 [index] 时作为冷备份节点运行：
// 1. 启动不扫描、不建索引、不预热正文缓存（内存极小、启动极快）
// 2. status 明确声明 index: disabled (cold backup)
// 3. recall / grep 调用明确报错拒收，禁止静默返回空结果（静默失败是最贵的故障）
// 4. Git 写入与命令流落地完全不受影响（Git 是真相源）
func TestIndexDisabledByDefault(t *testing.T) {
	// 1) 解析不带 [index] 的配置
	cfg, err := config.ParseFile([]byte(`
[server]
data = "/tmp/zm-cold-test"

[[user]]
name = "zero"
root = true
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Index.Enabled {
		t.Fatalf("没配 [index] 时默认不应启用索引（冷备份安全默认）")
	}

	// 2) 显式配 enabled = true
	cfgEnabled, err := config.ParseFile([]byte(`
[server]
data = "/tmp/zm-cold-test"

[[user]]
name = "zero"
root = true

[index]
enabled = true
`))
	if err != nil {
		t.Fatalf("parse enabled: %v", err)
	}
	if !cfgEnabled.Index.Enabled {
		t.Fatalf("配了 [index] enabled = true 时应启用索引")
	}

	// 3) 显式配 enabled = false
	cfgDisabled, err := config.ParseFile([]byte(`
[server]
data = "/tmp/zm-cold-test"

[[user]]
name = "zero"
root = true

[index]
enabled = false
`))
	if err != nil {
		t.Fatalf("parse disabled: %v", err)
	}
	if cfgDisabled.Index.Enabled {
		t.Fatalf("配了 [index] enabled = false 时不应启用索引")
	}
}

// 索引关闭时，落到本机的每一条路径都不许再进索引。
//
// 踩过的坑（2026-11-24 实测，rock64）：启动路径上拦住了索引，
// 但 refreshPath 没有守卫——接收命令流时逐条路径建索引，
// 容器内存从 3.97 MiB 涨到 1.547 GiB，CPU 95%，
// 而 status 上明明白白写着 "index: disabled"。
// 「配置说不建、代码还在建」比完全不建更坏：人以为省了。
func TestIndexDisabledSkipsRefreshPath(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	srv.indexEnabled = false
	// 换一个空索引，确保后续计数只反映新增
	srv.treesPtr.Store(&treeSet{trees: nil, idx: index.New()})

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 写一篇记忆：Git 必须落盘，索引必须一动不动
	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:cold.md $BODY",
			"vars": map[string]string{"BODY": "冷备份也要落盘"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 内容必须真的进了 Git（冷备份的唯一职责）
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := repo.Stat("zero", "cold.md"); !found {
		t.Fatalf("索引关闭不影响落盘：Git 里必须有 cold.md")
	}

	// 索引必须仍然是空的
	if got := srv.idxNow().Docs(); got != 0 {
		t.Fatalf("索引已关闭，不该有任何文档进索引，实际 %d 篇——"+
			"refreshPath 漏了守卫，会让冷备份节点悄悄建回全量索引", got)
	}
}

// 验证未启用索引的实例上，调用 recall / grep 必须明确报错，禁止静默返回空
func TestColdBackupRefusesQueryVerbsLoudly(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	// 关闭索引
	srv.indexEnabled = false

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// grep 必须报错
	_, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd": "grep something",
		},
	})
	if err == nil {
		t.Fatalf("未启用索引时 grep 必须报错出声，禁止静默返回空")
	}
	if !strings.Contains(err.Error(), "index is disabled") {
		t.Fatalf("报错信息必须说明原因（index is disabled），实际: %v", err)
	}

	// recall 必须报错
	_, err = callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd": "recall something",
		},
	})
	if err == nil {
		t.Fatalf("未启用索引时 recall 必须报错出声")
	}
	if !strings.Contains(err.Error(), "index is disabled") {
		t.Fatalf("报错信息必须说明原因，实际: %v", err)
	}

	// status 必须明确打印 index: disabled
	txt, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd": "status",
		},
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(txt, "index: disabled") {
		t.Fatalf("status 必须显示 index: disabled，实际:\n%s", txt)
	}
}
