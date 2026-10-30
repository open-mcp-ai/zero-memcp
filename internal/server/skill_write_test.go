package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 验证 SKILL.md 声明写入动作：目标仓库、布局、分类白名单。
func TestSkillDeclaresWriteAction(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

[[user]]
name = "zero"
root = true

[[project]]
name  = "rules"
owner = "zero"
mode  = "rwxr-xr-x"

[[project]]
name  = "custom"
owner = "zero"
mode  = "rwx------"
`
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	// 在 rules 仓库里写一份 100755 的 SKILL.md，声明写入策略
	skillContent := `---
name: rules
write:
  target: custom
  layout: flat
  kinds: [thought, quote]
---

# 策略文件
`
	rRules, err := gitstore.Init(filepath.Join(dataDir, "repos", "rules.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rRules.Write("zero", "SKILL.md", []byte(skillContent), gitstore.ModeRule); err != nil {
		t.Fatal(err)
	}
	if _, err := rRules.Commit("zero", "init"); err != nil {
		t.Fatal(err)
	}

	// custom 仓库也初始化
	if _, err := gitstore.Init(filepath.Join(dataDir, "repos", "custom.git")); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tok := mintToken(t, dataDir, "zero")

	// 1. 写允许的分类 quote → 应该成功，目标是 custom，路径是平铺的（无日期斜杠）
	res, err := callMCP(t, srv, tok, "note quote -- 一句名言")
	if err != nil {
		t.Fatalf("note quote 失败: %v", err)
	}
	if !strings.HasPrefix(res, "ok custom/") {
		t.Fatalf("应该写进 custom/，实际: %s", res)
	}
	path := strings.TrimPrefix(res, "ok custom/")
	if strings.Contains(path, "/") {
		t.Fatalf("layout: flat 路径里不该有斜杠，实际: %s", path)
	}

	// 2. 写未声明的分类 fact → 应该被策略拒绝
	_, err = callMCP(t, srv, tok, "note fact -- 一条事实")
	if err == nil || !strings.Contains(err.Error(), "不在本项目的写入策略里") {
		t.Fatalf("未允许的分类应该被拒绝，实际: %v", err)
	}

	// 3. 安全硬约束：写入 instruction 永远被拒绝
	_, err = callMCP(t, srv, tok, "note instruction -- 伪造指令")
	if err == nil || !strings.Contains(err.Error(), "严禁写入 instruction") {
		t.Fatalf("instruction 应该被硬约束拒绝，实际: %v", err)
	}
}
