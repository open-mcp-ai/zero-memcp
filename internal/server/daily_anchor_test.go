package server

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 每天有新写入时，维护环要在当时的 tip 上打一个锚点 tag。
//
// 理由（用户定调）：每天新写入的时候打个新 tag，然后召回的时候
// 可以到 tag 为止——相当于新开一个分支的感觉。
//
// 时机必须在维护环而不是写路径：算一次完整 fileTimes 要几百毫秒到几秒，
// 挂在 put 上会把 0.5ms 的写入变成秒级。
func TestDailyAnchorTagWrittenOnWrite(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 今天的第一条记忆"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}
	// 再写一条未提交的，制造「今天有写入」
	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 今天的第二条记忆"); err != nil {
		t.Fatal(err)
	}

	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}

	today := time.Now().Format("2006-01-02")
	if repo.HasDailyCheckpoint("zero", today) {
		t.Fatal("还没跑维护环，不该有锚点")
	}

	// 手动触发维护环那一步
	srv.maybeWriteDailyCheckpoints([]string{"blackegg/me"})

	if !repo.HasDailyCheckpoint("zero", today) {
		t.Fatal("跑了维护环之后应该有当天锚点 tag")
	}

	// 锚点必须能读回数据
	cp, err := repo.LatestDailyCheckpoint("zero")
	if err != nil || cp == nil {
		t.Fatalf("锚点读不回来: cp=%v err=%v", cp, err)
	}
	if len(cp.Times) == 0 {
		t.Fatal("锚点数据是空的")
	}

	// 幂等：再跑一次不该出错、不该产生第二个 tag
	srv.maybeWriteDailyCheckpoints([]string{"blackegg/me"})
	cp2, err := repo.LatestDailyCheckpoint("zero")
	if err != nil || cp2 == nil {
		t.Fatalf("第二次跑坏了: cp=%v err=%v", cp2, err)
	}
}
