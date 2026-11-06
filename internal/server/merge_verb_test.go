package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// TestMergeVerbE2E 验证真实链路：
//
//	两台机器各自写记录 → 机器 A 把远端 fetch 下来 →
//	执行 merge daily main.record from backup →
//	分叉历史合并成两亲 commit，双方记忆都在。
func TestMergeVerbE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dataDir := t.TempDir()
	// 用一个 bare repo 模拟对端机器
	remoteDir := filepath.Join(t.TempDir(), "backup.git")
	runGit(t, t.TempDir(), "init", "--bare", remoteDir)

	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project = "daily"
name    = "backup"
url     = %q
push    = ["main.record"]
`, dataDir, remoteDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	// 本地仓库
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}

	// 1. 本地写种子提交并推给远端作为共同祖先
	if err := repo.Write("main.record", "seed.md", []byte("seed\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main.record", "seed"); err != nil {
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

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})

	// 先推 seed 到远端
	if _, err := callMCP(t, srv, tok, "push daily backup main.record"); err != nil {
		t.Fatalf("push seed: %v", err)
	}

	// 2. 本地写一条 laptop 的记录
	if err := repo.Write("main.record", "record/laptop.md", []byte("from laptop\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main.record", "laptop note"); err != nil {
		t.Fatal(err)
	}

	// 3. 模拟对端机器在 remoteDir 上写了一条 desktop 的记录（用系统 git 提交到 bare 仓库的临时克隆）
	peerClone := t.TempDir()
	runGit(t, peerClone, "clone", remoteDir, ".")
	runGit(t, peerClone, "checkout", "main.record")
	if err := os.MkdirAll(filepath.Join(peerClone, "record"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := osWrite(filepath.Join(peerClone, "record", "desktop.md"), "from desktop\n"); err != nil {
		t.Fatal(err)
	}
	runGit(t, peerClone, "add", "-A")
	runGit(t, peerClone, "commit", "-m", "desktop note")
	runGit(t, peerClone, "push", "origin", "main.record")

	// 4. fetch 远端（此时两边分叉）
	out, err := callMCP(t, srv, tok, "fetch daily backup")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(out, "main.record") {
		t.Fatalf("fetch 输出应包含 main.record 分支: %s", out)
	}

	// 5. sync 应报告 diverged
	sout, err := callMCP(t, srv, tok, "sync daily backup")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !strings.Contains(sout, "diverged") {
		t.Fatalf("两边分叉后 sync 应报 diverged: %s", sout)
	}

	// 6. 执行 merge
	mout, err := callMCP(t, srv, tok, "merge daily main.record from backup")
	if err != nil {
		t.Fatalf("merge 动词失败: %v", err)
	}
	if !strings.Contains(mout, "merged") {
		t.Fatalf("merge 应成功输出 merged: %s", mout)
	}

	// 7. 验证合并后本地分支包含两边的内容
	for _, p := range []string{"seed.md", "record/laptop.md", "record/desktop.md"} {
		d, rerr := repo.Read("main.record", p)
		if rerr != nil {
			t.Errorf("合并后缺少 %s: %v", p, rerr)
		} else if len(d) == 0 {
			t.Errorf("%s 内容为空", p)
		}
	}

	// 8. 权限拦截：只读 token 调 merge 应被拒绝
	roTok := mintRestricted(t, dataDir, auth.Claims{Sub: "visitor", Name: "guest", Cap: "r"})
	if _, err := callMCP(t, srv, roTok, "merge daily main.record from backup"); err == nil {
		t.Fatal("只读 token 执行 merge 应被拒绝，却成功了")
	}
}
