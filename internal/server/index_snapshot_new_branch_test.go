package server

import (
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 快照之后新建的分支必须进入启动后的索引。
//
// 快照只保存当时存在的树。恢复时如果仅以「快照里有哪些树」作为当前
// 分支集合，新分支不会触发 head 差量，也不会被全量扫描；它的数据在
// Git 里、read 能读，recall/grep 却永远看不到。
func TestSnapshotRestoreIncludesBranchesCreatedAfterSnapshot(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero:before.md -- before-snapshot"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m before"); err != nil {
		t.Fatal(err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close first server: %v", err)
	}

	// 快照落盘以后，绕开旧 Server 直接创建一条新分支并提交一篇记忆。
	repo, err := gitstore.Open(filepath.Join(dataDir, "repos", "blackegg", "me.git"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "zzz-post-snapshot-branch"
	if err := repo.Write("after-snapshot", "new.md", []byte(marker), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("after-snapshot", "new branch after snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })

	out, err := callMCP(t, restarted, tok, "recall "+marker+" --scope blackegg/me@after-snapshot")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(out, marker) {
		t.Fatalf("快照后新建分支没有进入索引；数据已提交但 recall 看不到：%s", out)
	}
}
