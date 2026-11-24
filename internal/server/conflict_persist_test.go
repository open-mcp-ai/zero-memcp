package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 冲突必须落盘，并且不许被别的对端盖掉。
//
// 群内评审第 2、3 条：
//
//	2. 冲突只在内存，重启后 status 不再报冲突，但冲突还躺在盘上——
//	   本地那份没被覆盖、远端那份也在对象库，就是没人知道还欠着裁决。
//	3. 同一条路径只留一条冲突记录，三台一起改同一路径时后到的盖掉先到的。
func TestWorkConflictPersistsAcrossRestart(t *testing.T) {
	srv, data := setupMountServer(t)
	_ = data

	now := time.Now()
	// 两个对端对同一分支的同一路径各报一条冲突。
	srv.recordWorkConflict("p/one", "main", "notes/a.md", "aaa", "bbb", "peer-A", ConflictSet)
	srv.recordWorkConflict("p/one", "main", "notes/a.md", "aaa", "ccc", "peer-B", ConflictSet)

	got := srv.workConflicts()
	if len(got) != 2 {
		t.Fatalf("两个对端的冲突必须各留一条，实际 %d 条: %+v", len(got), got)
	}
	peers := map[string]bool{}
	for _, c := range got {
		peers[c.Peer] = true
	}
	if !peers["peer-A"] || !peers["peer-B"] {
		t.Fatalf("两方证据都要在，实际: %+v", got)
	}
	_ = now

	// 落盘并换一个实例加载：重启后必须仍然看得见。
	if err := srv.saveOpSyncState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	srv2, data2 := setupMountServer(t)
	raw, err := os.ReadFile(srv.opSyncStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srv2.opSyncStatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv2.loadOpSyncState(); err != nil {
		t.Fatalf("load: %v", err)
	}
	after := srv2.workConflicts()
	if len(after) != 2 {
		t.Fatalf("重启后必须仍有 2 条冲突，实际 %d 条", len(after))
	}
	token2 := mintRestricted(t, data2, auth.Claims{Sub: "zero", Cap: "rwx"})
	out, err := callMCP(t, srv2, token2, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "a.md") {
		t.Fatalf("重启后 status 必须仍报出待裁决路径，实际:\n%s", out)
	}

	// 只清 peer-A：peer-B 的账不能被替它销掉。
	srv2.clearWorkConflict("p/one", "main", "peer-A", "notes/a.md")
	rest := srv2.workConflicts()
	if len(rest) != 1 || rest[0].Peer != "peer-B" {
		t.Fatalf("清掉 peer-A 后应只剩 peer-B，实际: %+v", rest)
	}
}

// Git 层冲突同样要落盘、同样不许被别的 remote 盖掉。
func TestGitConflictPersistsAndKeepsRemotes(t *testing.T) {
	srv, data := setupMountServer(t)
	_ = data

	srv.addSyncConflict(syncConflict{
		Project: "p/one", Branch: "main", Remote: "hub-a",
		Paths: []string{"x.md"}, At: time.Now(),
	})
	srv.addSyncConflict(syncConflict{
		Project: "p/one", Branch: "main", Remote: "hub-b",
		Paths: []string{"y.md"}, At: time.Now(),
	})
	// 同一个 remote 再来一次：路径取并集，不许把前一次覆盖掉。
	srv.addSyncConflict(syncConflict{
		Project: "p/one", Branch: "main", Remote: "hub-a",
		Paths: []string{"z.md"}, At: time.Now(),
	})

	all := srv.SyncConflicts()
	if len(all) != 2 {
		t.Fatalf("两个 remote 必须各留一条，实际 %d 条: %+v", len(all), all)
	}
	byRemote := map[string][]string{}
	for _, c := range all {
		byRemote[c.Remote] = c.Paths
	}
	if len(byRemote["hub-a"]) != 2 {
		t.Fatalf("同一 remote 再次冲突时路径应取并集（x.md + z.md），实际: %v", byRemote["hub-a"])
	}

	if err := srv.saveOpSyncState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	srv2, data2 := setupMountServer(t)
	raw, _ := os.ReadFile(srv.opSyncStatePath())
	if err := os.WriteFile(srv2.opSyncStatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv2.loadOpSyncState(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := len(srv2.SyncConflicts()); n != 2 {
		t.Fatalf("重启后 Git 层冲突必须仍有 2 条，实际 %d", n)
	}
	_ = data2
}
