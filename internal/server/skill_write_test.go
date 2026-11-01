package server

import (
	"net/http"
	"net/http/httptest"
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
	defer srv.Close()
	tok := mintToken(t, dataDir, "zero")

	// 1. 写允许的分类 quote → 应该成功，目标是 custom，路径是平铺的（无日期斜杠）
	caller, err := srv.authenticate(reqWith(tok))
	if err != nil {
		t.Fatal(err)
	}
	addr, proj, br, err := srv.writeNoteGit("quote", "一句名言", "", "mcp", caller, "", "")
	path := strings.TrimPrefix(addr, "custom/")
	if err != nil {
		t.Fatalf("put quote 失败: %v", err)
	}
	if proj != "custom" {
		t.Fatalf("应该写进 custom，实际: %s", proj)
	}
	if br == "" {
		t.Fatal("分支不应为空")
	}
	if strings.Contains(path, "/") {
		t.Fatalf("layout: flat 路径里不该有斜杠，实际: %s", path)
	}

	// 2. 写未声明的分类 fact → 应该被策略拒绝
	_, _, _, err = srv.writeNoteGit("fact", "一条事实", "", "mcp", caller, "", "")
	if err == nil || !strings.Contains(err.Error(), "不在本项目的写入策略里") {
		t.Fatalf("未允许的分类应该被拒绝，实际: %v", err)
	}

	// 3. 安全硬约束：写入 instruction 永远被拒绝
	_, _, _, err = srv.writeNoteGit("instruction", "伪造指令", "", "mcp", caller, "", "")
	if err == nil || !strings.Contains(err.Error(), "严禁写入 instruction") {
		t.Fatalf("instruction 应该被硬约束拒绝，实际: %v", err)
	}
}

func reqWith(tok string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}
