package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// issue #9：
//  1. grep 在「只由 ACL 授予、不在挂载表里」的分支（如 @ops / @main.shared）上
//     命中头不能是空的 `--  --`，必须回显一个能直接喂给 read 的完整地址；
//  2. 子树 chroot 之外的兄弟路径绝不能因为改用 virtualPath 而泄漏；
//  3. 增加 `-l` / `--files-only` 只列命中文件，且在 help grep 里把 -i / -c / -l
//     全部文档化。
func TestIssue9GrepShowsFilenameOnACLBranchAndSupportsFilesOnly(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[index]
enabled = true

[[user]]
name = "zero"
root = true

[[user]]
name = "weft"

[[project]]
name  = "BlackEggAI/weft"
owner = "zero"
mode  = "rwxr-xr-x"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", filepath.ToSlash(dataDir))
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "BlackEggAI", "weft.git"))
	if err != nil {
		t.Fatal(err)
	}
	// 主线一个挂载分支（sub/inside vs sub/secret 测试 chroot），
	// 再加一条 ops 分支（通过 ACL 授给 weft，不在 token 的 Mount 表里——
	// 正是 issue #9 复现里掉进 `--  --` 的那一种）。
	if err := repo.Write("zero", "allowed/note.md", []byte("关键词-UNIQUE9 挂载可见\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "secret/hidden.md", []byte("关键词-UNIQUE9 越狱不可见\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed main"); err != nil {
		t.Fatal(err)
	}

	if err := repo.Fork("zero", "ops"); err != nil {
		t.Fatal(err)
	}
	// 清掉继承的 secret，写入 ops 专属运维清单。
	_ = repo.Delete("ops", "secret/hidden.md")
	_ = repo.Delete("ops", "allowed/note.md")
	if err := repo.Write("ops", "ports/1729.md", []byte("port=1729 关键词-UNIQUE9\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("ops", "ports/8080.md", []byte("port=8080 关键词-UNIQUE9\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("ops", "seed ops"); err != nil {
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
	t.Cleanup(func() { _ = srv.Close() })

	// 管理员给 weft 授予 BlackEggAI/weft@ops 只读权限（纯 ACL，不在 Mount 表）。
	// ACL 按凭证名登记，先把 weft 这张凭证落进花名册（与真实实例一致）。
	if err := auth.SaveTokenRecord(dataDir, auth.TokenRecord{
		JTI: "jti-weft-9", Name: "weft", Sub: "weft",
	}); err != nil {
		t.Fatal(err)
	}
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	if _, e := rawTerm(t, srv, rootTok, "acl put weft BlackEggAI/weft@ops r", nil); e != "" {
		t.Fatalf("授予 ACL 失败: %s", e)
	}

	// weft 的挂载表：zero 分支收窄到 allowed 子树；ops 单独挂在自己的虚拟根。
	//
	// 注：早先这里的 ops 是「只由 ACL 授权、完全不在挂载表里」——那种形态在
	// 「token 是选择范围」的不变式下已不存在（ACL 不能越过挂载表）。
	// ACL 仍是一层有效判定，只是它只能在天花板之内收窄/放行。
	weftTok := mintRestricted(t, dataDir, auth.Claims{
		Sub:  "weft",
		Name: "weft",
		Cap:  "rw",
		Mount: []auth.MountPoint{
			{Project: "BlackEggAI/weft", Branch: "zero", Path: "allowed", At: "/"},
			{Project: "BlackEggAI/weft", Branch: "ops", At: "/ops", Cap: "r"},
		},
	})

	// ① grep @ops 必须带上可定位地址，绝不允许出现空的 `--  --`。
	out, e := rawTerm(t, srv, weftTok, "grep 关键词-UNIQUE9 --scope BlackEggAI/weft@ops", nil)
	if e != "" {
		t.Fatalf("grep @ops 失败: %s", e)
	}
	if strings.Contains(out, "--  --") {
		t.Fatalf("ACL 分支命中头不能是空的 `--  --`:\n%s", out)
	}
	const expectedAddr = "ops/ports/1729.md"
	if !strings.Contains(out, "-- "+expectedAddr+" --") {
		t.Fatalf("应回显可定位路径 %q，实际:\n%q", expectedAddr, out)
	}
	// 回显的地址必须真的能直接喂给 read。
	rd, e := rawTerm(t, srv, weftTok, "read "+expectedAddr, nil)
	if e != "" || !strings.Contains(rd, "port=1729") {
		t.Fatalf("grep 返回的地址喂给 read 应能读出正文，实际 err=%q out=%q", e, rd)
	}

	// ② chroot 隔离绝不能退化：main 分支下的 secret/hidden.md 不能出现在结果里。
	allOut, e := rawTerm(t, srv, weftTok, "grep 关键词-UNIQUE9", nil)
	if e != "" {
		t.Fatalf("全局 grep 失败: %s", e)
	}
	if strings.Contains(allOut, "越狱不可见") {
		t.Fatalf("chroot 子树外的兄弟文件绝不能泄漏:\n%s", allOut)
	}
	if !strings.Contains(allOut, "-- note.md --") {
		t.Fatalf("挂载子树内的文件应保留虚拟路径:\n%s", allOut)
	}

	// ③ -l / --files-only 只列文件路径，每块一行，不含正文字面行。
	lOut, e := rawTerm(t, srv, weftTok, "grep -l 关键词-UNIQUE9 --scope BlackEggAI/weft@ops", nil)
	if e != "" {
		t.Fatalf("grep -l 失败: %s", e)
	}
	if strings.Contains(lOut, "port=") {
		t.Fatalf("grep -l 不应带正文字面内容:\n%s", lOut)
	}
	if !strings.Contains(lOut, "ops/ports/1729.md") ||
		!strings.Contains(lOut, "ops/ports/8080.md") {
		t.Fatalf("grep -l 应列出两份命中文件:\n%s", lOut)
	}
	lLongOut, e := rawTerm(t, srv, weftTok, "grep --files-only 关键词-UNIQUE9 --scope BlackEggAI/weft@ops", nil)
	if e != "" || lLongOut != lOut {
		t.Fatalf("--files-only 应与 -l 完全等价: err=%q\n-l=%q\n--files-only=%q", e, lOut, lLongOut)
	}

	// ④ -c 与 -l 互斥报错；help grep 把 -i / -c / -l 都讲清楚。
	if _, e := rawTerm(t, srv, weftTok, "grep -c -l 关键词-UNIQUE9", nil); e == "" {
		t.Fatal("-c 与 -l 同时传入应该被拒绝")
	}
	hOut, _ := rawTerm(t, srv, weftTok, "help grep", nil)
	for _, flag := range []string{"-i", "-c", "-l", "--files-only"} {
		if !strings.Contains(hOut, flag) {
			t.Fatalf("help grep 应文档化 %s:\n%s", flag, hOut)
		}
	}
}
