package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 分身边界：凭证读 main、只写 chat。
//
// 这是「分身不许脏核心」的唯一配置形态，之前实测写入被拒
// （missing w permission），此测试锁死它必须成功。
func TestScopedAgentWritesChatBranch(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

[[user]]
name = "zero"
root = true

[[project]]
name  = "BlackEggAI/zero"
owner = "zero"
mode  = "rwx------"
`
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(dataDir, "repos", "BlackEggAI", "zero.git")
	repo, err := gitstore.Init(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main", "seed.md", []byte("seed"), gitstore.ModeContent); err != nil {
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

	tok, err := srv.tokens.Sign(auth.Claims{
		Sub:  "zero",
		Name: "agent-chat",
		Cap:  "rw",
		Mount: []auth.MountPoint{
			{Project: "BlackEggAI/zero", Branch: "main", Cap: "r"},
			{Project: "BlackEggAI/zero", Branch: "main.record", Cap: "r"},
			{Project: "BlackEggAI/zero", Branch: "chat", Cap: "rw"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := srv.authenticate(reqWith(tok))
	if err != nil {
		t.Fatal(err)
	}
	// chat 分支先建出来（基于 main），否则写入落点解析不到
	if err := repo.Write("chat", "seed.md", []byte("seed"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}

	addr, proj, br, err := srv.writeNoteGit("", "分身写的一条", "", "mcp", caller, "BlackEggAI/zero@chat", "")
	if err != nil {
		t.Fatalf("写 chat 被拒: %v", err)
	}
	if br != "chat" {
		t.Fatalf("落点分支 = %q, want chat（addr=%s proj=%s）", br, addr, proj)
	}
	_ = addr

	// 反例：写 main 必须被拒
	_, _, _, err = srv.writeNoteGit("", "不该成功", "", "mcp", caller, "BlackEggAI/zero@main", "")
	if err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("写 main 应被拒且报权限，实际 err=%v", err)
	}
}
