package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
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
