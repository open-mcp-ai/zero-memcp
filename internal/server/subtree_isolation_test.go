package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// TestSubtreeIsolation 验证简的多个分身互不可见的实现：
//
//	简-贴吧分身  挂载 zero@main.record:record/tieba  (rw)
//	简-群聊分身  挂载 zero@main.record:record/group  (rw)
//
// 验证四件事：
//  1. 贴吧分身写内容，落点真实路径在 record/tieba 下
//  2. 群聊分身读贴吧的地址，返回 outside mount's subtree（不可见）
//  3. 贴吧分身做 branch/tag/reset 等分支操作被 requireBranchWide 拦截
//  4. 贴吧分身调 ls，只能看到自己子树下的条目
func TestSubtreeIsolation(t *testing.T) {
	dataDir := t.TempDir()
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[project]]
name  = "BlackEggAI/zero"
owner = "zero"
mode  = "rwx------"
`, dataDir)
	if err := osWrite(dataDir+"/zero-memcp.toml", cfg); err != nil {
		t.Fatal(err)
	}

	repo, err := gitstore.Init(dataDir + "/repos/BlackEggAI/zero.git")
	if err != nil {
		t.Fatal(err)
	}
	// 在群聊和贴吧各自种一颗种子
	if err := repo.Write("main.record", "record/group/chat1.md", []byte("group secret"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("main.record", "record/tieba/post1.md", []byte("tieba post"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main.record", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  dataDir + "/zero-memcp.toml",
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// 签发两张子路径收窄凭证
	tiebaToken := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "zero-tieba", Cap: "rw",
		Mount: []auth.MountPoint{
			{Project: "BlackEggAI/zero", Branch: "main.record", Path: "record/tieba", At: "/tieba", Cap: "rw"},
		},
	})
	groupToken := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Name: "zero-group", Cap: "rw",
		Mount: []auth.MountPoint{
			{Project: "BlackEggAI/zero", Branch: "main.record", Path: "record/group", At: "/group", Cap: "rw"},
		},
	})

	// 1. 贴吧分身读自己的内容：可读
	out, err := callMCP(t, srv, tiebaToken, "read /tieba/post1.md")
	if err != nil || !strings.Contains(out, "tieba post") {
		t.Fatalf("贴吧分身读自己内容应成功: %s, err: %v", out, err)
	}

	// 2. 贴吧分身越界读群聊内容：不可见
	out, rerr := callMCP(t, srv, tiebaToken, "read BlackEggAI/zero@main.record:record/group/chat1.md")
	msg := out
	if rerr != nil {
		msg += " " + rerr.Error()
	}
	if !strings.Contains(msg, "outside this mount's subtree") {
		t.Fatalf("跨子树读应被拦截为 outside，实际 out=%q err=%v", out, rerr)
	}

	// 3. 群聊分身越界读贴吧内容：不可见
	out, rerr = callMCP(t, srv, groupToken, "read BlackEggAI/zero@main.record:record/tieba/post1.md")
	msg = out
	if rerr != nil {
		msg += " " + rerr.Error()
	}
	if !strings.Contains(msg, "outside this mount's subtree") {
		t.Fatalf("群聊分身越界读应被拦截，实际 out=%q err=%v", out, rerr)
	}

	// 4. 贴吧分身执行 branch 操作被拦截
	_, err = callMCP(t, srv, tiebaToken, "branch BlackEggAI/zero newbranch")
	if err == nil || !strings.Contains(err.Error(), "mounted with a path subtree") {
		t.Fatalf("路径受限凭证做分支操作应被拦截: %v", err)
	}

	// 5. 贴吧分身调 ls，不应看见群聊内容
	lsOut, err := callMCP(t, srv, tiebaToken, "ls")
	if err != nil {
		t.Fatalf("ls 失败: %v", err)
	}
	if strings.Contains(lsOut, "group") || strings.Contains(lsOut, "chat1") {
		t.Fatalf("贴吧分身的 ls 里泄漏了群聊内容: %s", lsOut)
	}
	if !strings.Contains(lsOut, "post1.md") {
		t.Fatalf("贴吧分身的 ls 里应包含自己的内容: %s", lsOut)
	}
}
