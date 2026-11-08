package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// TestAutoSyncE2E 验证完整的自动对等同步闭环：
//
//	模拟 GitHub 裸仓库作中心
//	  ↓
//	机器 A 本地写入 → 自动提交 → kickSync → 自动推给 GitHub
//	  ↓
//	机器 B （模拟对端）在 GitHub 上写了一条记录
//	  ↓
//	机器 A 自动 fetch → merge (正交两亲提交) → 自动推回 GitHub
//	  ↓
//	机器 A 验证双方内容都完好
func TestAutoSyncE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dataDir := t.TempDir()
	// 用一个本地 bare 仓库扮演 GitHub 上的 BlackEggAI/daily
	hubDir := filepath.Join(t.TempDir(), "hub.git")
	runGit(t, t.TempDir(), "init", "--bare", hubDir)

	cfg := fmt.Sprintf(`
[server]
data = %q

[commit]
interval = "100ms"
count    = 1

[sync]
interval = "200ms"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project   = "daily"
name      = "github"
url       = %q
push      = ["main.record"]
auto_sync = true
`, dataDir, hubDir)
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// 本地仓库先建好并种下 seed，推到 GitHub 作为初始基线
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
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
	// Start 阻塞直到 Close，放后台跑。
	go func() { _ = srv.Start() }()
	defer srv.Close()

	// 1. 本地写入达到 count=1 → maintain.go 折叠 → kickSync → 推给 GitHub
	// 用 put 动词写（走真实写路径）
	_, err = srv.execTermVars("put daily@main.record/record/laptop-1.md $1", nil, []string{"written from laptop"}, Viewer{}, rootCaller())
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	// 等自动提交 + 自动推送到达 GitHub
	waitForHubHasFile(t, hubDir, "main.record", "record/laptop-1.md", 25*time.Second)

	// 2. 模拟 3060（对端机器）克隆 GitHub 并写了一条 desktop 记录推回 GitHub
	peerClone := t.TempDir()
	runGit(t, peerClone, "clone", hubDir, ".")
	runGit(t, peerClone, "checkout", "main.record")
	if err := os.MkdirAll(filepath.Join(peerClone, "record"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peerClone, "record", "desktop-1.md"), []byte("from desktop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, peerClone, "add", "-A")
	runGit(t, peerClone, "commit", "-m", "from desktop")
	runGit(t, peerClone, "push", "origin", "main.record")

	// 3. 此时两边分叉。机器 A 的 sync ticker 会在 200ms 内触发：
	//    fetch github → merge (正交生成两亲 commit) → push 回 GitHub
	// 等机器 A 自身把 desktop-1.md 合并进来
	waitForLocalHasFile(t, repo, "main.record", "record/desktop-1.md", 25*time.Second)

	// 4. 机器 A 应该同时拥有两边的记录。
	// put 写出的是目录形态（record/laptop-1.md/<切片文件>），
	// 所以按前缀判存在，而不是读某个具体文件。
	entries, lerr := repo.List("main.record")
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	hasPrefix := func(prefix string) bool {
		for _, e := range entries {
			if e.Path == prefix || strings.HasPrefix(e.Path, prefix+"/") {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"seed.md", "record/laptop-1.md", "record/desktop-1.md"} {
		if !hasPrefix(want) {
			t.Errorf("合并后缺少 %s；现有：%v", want, func() []string {
				var ps []string
				for _, e := range entries {
					ps = append(ps, e.Path)
				}
				return ps
			}())
		}
	}

	// 5. 验证 sync 命令的状态输出包含 auto-sync 信息
	syncOut, serr := srv.execSync("daily github", rootCaller())
	if serr != nil {
		t.Fatalf("sync 动词: %v", serr)
	}
	if !strings.Contains(syncOut.Text, "auto-sync: last") {
		t.Errorf("sync 输出应包含 auto-sync 时间: %s", syncOut.Text)
	}
}

func waitForHubHasFile(t *testing.T, bareDir, branch, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("git", "archive", "--remote="+bareDir, branch, path)
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 GitHub 远端出现 %s@%s:%s 超时", bareDir, branch, path)
}

func waitForLocalHasFile(t *testing.T, repo *gitstore.Repo, branch, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if entries, err := repo.List(branch); err == nil {
			for _, e := range entries {
				if e.Path == path || strings.HasPrefix(e.Path, path+"/") {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待本地分支合并 %s@%s 超时", branch, path)
}

// TestSyncConflictNotSilent 锁定冲突场景：
//
//	两边对同一个文件写了不同的内容（这是真冲突，不是正交新增）
//	→ 自动同步不得用任何一边覆盖另一边
//	→ 冲突进队列，sync 状态里常驻显示
//	→ 指针停在本地，不推、不猜
func TestSyncConflictNotSilent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	hubDir := filepath.Join(t.TempDir(), "hub.git")
	runGit(t, t.TempDir(), "init", "--bare", hubDir)

	// 中心先有共同祖先：SKILL.md v1
	seed := t.TempDir()
	runGit(t, seed, "clone", hubDir, ".")
	runGit(t, seed, "checkout", "-b", "main.record")
	if err := os.MkdirAll(filepath.Join(seed, "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "main", "SKILL.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "v1")
	runGit(t, seed, "push", "origin", "main.record")

	cfg := fmt.Sprintf(`
[server]
data = %q

[sync]
interval = "200ms"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project   = "daily"
name      = "github"
url       = %q
push      = ["main.record"]
auto_sync = true
`, dataDir, hubDir)
	os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o644)

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	// 本地从祖先分叉：本机写 SKILL.md = "laptop wins"
	if err := repo.Write("main.record", "main/SKILL.md", []byte("laptop wins\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("main.record", "laptop edit"); err != nil {
		t.Fatal(err)
	}
	// 先推一次本地基线？不行——那样远端就只有本机版本。
	// 直接进入测试：远端此时是 v1，本地是 "laptop wins"（本地 ahead）
	srv, err := New(Config{
		DataDir:    dataDir,
		ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:     "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()

	// 对端（另一台机器）基于 v1 写 "desktop wins" 推上 GitHub
	peer := t.TempDir()
	runGit(t, peer, "clone", hubDir, ".")
	runGit(t, peer, "checkout", "main.record")
	if err := os.WriteFile(filepath.Join(peer, "main", "SKILL.md"), []byte("desktop wins\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, peer, "add", "-A")
	runGit(t, peer, "commit", "-m", "desktop edit")
	runGit(t, peer, "push", "origin", "main.record")

	// 现在：祖先=v1，远端="desktop wins"，本地="laptop wins"。
	// 这是三方分叉且同路径不同内容 = 结构性真冲突。
	// 等自动同步跑起来，它必须：
	//   1. 不推任何一边（指针不被静默推进）
	//   2. 把冲突记进队列
	deadline := time.Now().Add(8 * time.Second)
	var conflicted bool
	for time.Now().Before(deadline) {
		if cs := srv.SyncConflicts(); len(cs) > 0 {
			conflicted = true
			if cs[0].Branch != "main.record" {
				t.Fatalf("冲突应记在 main.record，实际 %+v", cs[0])
			}
			if len(cs[0].Paths) == 0 || cs[0].Paths[0] != "main/SKILL.md" {
				t.Fatalf("冲突路径应为 main/SKILL.md，实际 %v", cs[0].Paths)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !conflicted {
		t.Fatal("8 秒内未看到冲突记录：要么合并被静默跳过，要么一方被静默覆盖——都不允许")
	}

	// 本地内容必须还是本机写的（谁也没覆盖谁）
	got, err := repo.Read("main.record", "main/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "laptop wins\n" {
		t.Fatalf("冲突期间本地内容被改动: %q", got)
	}
	// sync 状态里要能看到这条冲突
	out, err := srv.execSync("daily github", rootCaller())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "CONFLICTS") || !strings.Contains(out.Text, "main/SKILL.md") {
		t.Errorf("sync 状态应显示冲突:\n%s", out.Text)
	}
}
