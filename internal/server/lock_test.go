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

// 锁住之后：非 root 读写全拒，root 一切照常。
func TestLockBlocksNonRootOnly(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	// 一张挂在 chat 分支上的普通凭证
	agentTok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "agent-chat",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "chat"}},
	})

	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/a.md -- 待回收的内容"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m seed"); err != nil {
		t.Fatal(err)
	}

	// 锁之前 agent 能读
	if _, err := callMCP(t, srv, agentTok, "read blackegg/me@chat/a.md"); err != nil {
		t.Fatalf("锁之前应可读: %v", err)
	}

	out, err := callMCP(t, srv, rootTok, "lock blackegg/me@chat")
	if err != nil {
		t.Fatalf("lock 失败: %v", err)
	}
	if !strings.Contains(out, "已锁定") {
		t.Fatalf("lock 回显异常: %s", out)
	}

	// 非 root：读被拒
	readOut, readErr := callMCP(t, srv, agentTok, "read blackegg/me@chat/a.md")
	if readErr != nil {
		if !strings.Contains(readErr.Error(), "锁定") {
			t.Fatalf("拒绝理由应提到锁，实际: %v", readErr)
		}
	} else if !strings.Contains(readOut, "锁定") || strings.Contains(readOut, "待回收的内容") {
		t.Fatalf("锁住后读请求必须被拒绝且不能返回正文: %s", readOut)
	}
	// 非 root：写被拒
	if _, err := callMCP(t, srv, agentTok, "write blackegg/me@chat/b.md -- x"); err == nil {
		t.Fatal("锁住后非 root 不该能写")
	}
	// 非 root：put 被拒
	if _, err := callMCPWithVars(t, srv, agentTok, "put blackegg/me@chat -- $B", map[string]string{"B": "x"}); err == nil {
		t.Fatal("锁住后非 root 不该能 put")
	}

	// root：读、写、搬、归档全部照常
	if _, err := callMCP(t, srv, rootTok, "read blackegg/me@chat/a.md"); err != nil {
		t.Fatalf("锁住后 root 必须还能读（否则无法回收）: %v", err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/cleanup.md -- root 处理记录"); err != nil {
		t.Fatalf("锁住后 root 必须还能写（否则无法回收）: %v", err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m cleanup"); err != nil {
		t.Fatalf("锁住后 root 必须还能 commit: %v", err)
	}
}

// unlock 恢复原有权限，并如实报告锁定期间分支有没有动过。
func TestUnlockRestoresAccess(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	agentTok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "agent-chat",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "chat"}},
	})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/a.md -- x"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m seed"); err != nil {
		t.Fatal(err)
	}

	if _, err := callMCP(t, srv, rootTok, "lock blackegg/me@chat"); err != nil {
		t.Fatal(err)
	}
	out, err := callMCP(t, srv, rootTok, "unlock blackegg/me@chat")
	if err != nil {
		t.Fatalf("unlock 失败: %v", err)
	}
	if !strings.Contains(out, "已解锁") || !strings.Contains(out, "期间没有写入") {
		t.Fatalf("unlock 回显应说明期间有无写入: %s", out)
	}

	// 解锁后 agent 恢复
	if _, err := callMCP(t, srv, agentTok, "read blackegg/me@chat/a.md"); err != nil {
		t.Fatalf("解锁后应恢复访问: %v", err)
	}
	if _, err := callMCP(t, srv, agentTok, "write blackegg/me@chat/b.md -- 恢复写入"); err != nil {
		t.Fatalf("解锁后应恢复写入: %v", err)
	}

	// 重复 unlock 不报错
	again, err := callMCP(t, srv, rootTok, "unlock blackegg/me@chat")
	if err != nil {
		t.Fatalf("重复 unlock 不该报错: %v", err)
	}
	if !strings.Contains(again, "本来就没有锁") {
		t.Fatalf("重复 unlock 回显异常: %s", again)
	}
}

// 锁定期间 root 动过分支，unlock 必须说出来。
func TestUnlockReportsChangesDuringLock(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/a.md -- x"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "lock blackegg/me@chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/during.md -- 锁定期内写入"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m during"); err != nil {
		t.Fatal(err)
	}
	out, err := callMCP(t, srv, rootTok, "unlock blackegg/me@chat")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "推进到") {
		t.Fatalf("锁定期间有写入时必须报告: %s", out)
	}
}

// 锁是持久状态：重启（新 Server 实例）之后仍然生效。
func TestLockSurvivesRestart(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/a.md -- x"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "lock blackegg/me@chat"); err != nil {
		t.Fatal(err)
	}

	// 用同一个 dataDir 起一个新实例
	srv2, err := New(Config{DataDir: dataDir, ConfigFile: srv.cfg.ConfigFile})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()

	repo, err := srv2.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if locked, _ := repo.BranchLocked("chat"); !locked {
		t.Fatal("锁必须跨重启保留")
	}
}

// lock 的边界：非 root 不能锁；缺分支、不存在的分支必须报错。
func TestLockRejectsInvalid(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/a.md -- x"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m seed"); err != nil {
		t.Fatal(err)
	}

	nonRoot := mintRestricted(t, dataDir, auth.Claims{Sub: "bob", Name: "bob", Cap: "rwx"})
	if _, err := callMCP(t, srv, nonRoot, "lock blackegg/me@chat"); err == nil {
		t.Fatal("非 root 不该能锁分支")
	}
	if _, err := callMCP(t, srv, rootTok, "lock blackegg/me"); err == nil {
		t.Fatal("缺分支应报错")
	}
	if _, err := callMCP(t, srv, rootTok, "lock blackegg/me@nosuch"); err == nil {
		t.Fatal("锁一个不存在的分支应报错")
	}
}

// 被锁的分支在 branch 列表里要有标记。
func TestBranchListMarksLocked(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/a.md -- x"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "lock blackegg/me@chat"); err != nil {
		t.Fatal(err)
	}
	out, err := callMCP(t, srv, rootTok, "branch blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已锁定") {
		t.Fatalf("branch 列表应标出锁: %s", out)
	}
}

// 分支在锁定期间被 archive 掉之后，unlock 必须如实说「已不存在」，
// 不能谎报「停在原处」。
func TestUnlockAfterArchiveReportsMissing(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/a.md -- x"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "lock blackegg/me@chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "archive blackegg/me@chat"); err != nil {
		t.Fatal(err)
	}
	out, err := callMCP(t, srv, rootTok, "unlock blackegg/me@chat")
	if err != nil {
		t.Fatalf("unlock 不该报错: %v", err)
	}
	if !strings.Contains(out, "已不存在") {
		t.Fatalf("分支被归档后 unlock 必须说明它已不存在: %s", out)
	}
}

// 派生凭证（Sub 是 root，但被 mount 收窄）不能绕过锁。
//
// 这是「四[影]」的场景：root 为每个分身签一张挂在各自分支上的凭证。
// 如果锁只看 c.Root，那么所有分身都持有 Root=true，锁形同虚设。
func TestLockBlocksScopedRootDerivedTokens(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	derived := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "shadow-3",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "chat"}},
	})
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "write blackegg/me@chat/a.md -- 分身的内容"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, rootTok, "commit blackegg/me@chat -m seed"); err != nil {
		t.Fatal(err)
	}

	c, err := srv.authenticate(reqWith(derived))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Root {
		t.Fatal("测试前提：派生凭证的 Root 应为 true（Sub 是 root）")
	}

	if _, err := callMCP(t, srv, rootTok, "lock blackegg/me@chat"); err != nil {
		t.Fatal(err)
	}

	// 即便 Root=true，被收窄的凭证也必须被锁挡住
	out, readErr := callMCP(t, srv, derived, "read blackegg/me@chat/a.md")
	if readErr == nil && strings.Contains(out, "分身的内容") {
		t.Fatal("被收窄的 root 派生凭证不该能读到被锁分支的正文")
	}
	if _, err := callMCP(t, srv, derived, "write blackegg/me@chat/b.md -- x"); err == nil {
		t.Fatal("被收窄的 root 派生凭证不该能写被锁分支")
	}

	// 而真正不受限的 root 仍然可以
	if _, err := callMCP(t, srv, rootTok, "read blackegg/me@chat/a.md"); err != nil {
		t.Fatalf("不受限的 root 必须能读: %v", err)
	}
}

// git HTTP 也必须认锁：锁住的分支不能被派生凭证 clone 走。
func TestLockBlocksGitHTTPClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[listen]]
addr  = "127.0.0.1:0"
allow = ["*", "git-upload-pack"]

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwxrwxrwx"
`, dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "blackegg", "me.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.Write("chat", "secret.md", []byte("分身的秘密\n"), gitstore.ModeContent)
	if _, err := repo.Commit("chat", "seed"); err != nil {
		t.Fatal(err)
	}
	if locked, _ := repo.BranchLocked("chat"); locked {
		t.Fatal("测试前提：尚未加锁")
	}
	if _, err := repo.LockBranch("chat"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{DataDir: dataDir, ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"), EnforceAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()
	addr := waitForServerAddr(t, srv)

	// 不受限 root 能 clone 到内容
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	rootDir := filepath.Join(t.TempDir(), "root-clone")
	cmd := exec.Command("git", "clone", "-q", "-b", "chat", fmt.Sprintf("http://zero:%s@%s/repos/blackegg/me.git", rootTok, addr), rootDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("root clone 应成功: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "secret.md")); err != nil {
		t.Fatal("root 应看到被锁分支的内容")
	}

	// 派生凭证 clone 必须拿不到
	derived := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "shadow",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "chat"}},
	})
	derivedDir := filepath.Join(t.TempDir(), "shadow-clone")
	cmd = exec.Command("git", "clone", "-q", "-b", "chat", fmt.Sprintf("http://zero:%s@%s/repos/blackegg/me.git", derived, addr), derivedDir)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("派生凭证不该能 clone 被锁分支: %s", out)
	}
	if _, err := os.Stat(filepath.Join(derivedDir, "secret.md")); err == nil {
		t.Fatal("派生凭证拿到了被锁分支的正文")
	}
}
