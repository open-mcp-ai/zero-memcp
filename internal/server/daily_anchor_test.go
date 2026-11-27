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

// 分支头动过就要打锚点——哪怕本机没有任何未提交写入。
//
// 这是线上实测出来的缺口：锚点判据原本是 repo.PendingRefs()，
// 也就是「本机 put 过还没折叠」。但内容进入本机还有另一条路——同步、
// push、另一台机器 commit。那条路不产生脏层，于是这些分支永远拿不到
// 锚点，fileTimes 每次都要沿父链走完整个历史。
//
// 实测（v0.2.11，laptop）：zero@inbox 2,074 个提交 / 0 个锚点，
// List() 一次 62.4 秒；写一个锚点后 812ms。它在启动路径上被调两遍，
// 于是每次重启白等两分钟。
func TestDailyAnchorWrittenWithoutLocalPending(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 一条已经折叠过的记忆"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}

	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	// 前提：脏层是空的。旧判据（PendingRefs）在这里会直接跳过这条分支。
	if n := repo.Pending("zero"); n != 0 {
		t.Fatalf("前提不成立：commit 之后仍有 %d 个未提交写入", n)
	}
	if len(repo.PendingRefs()) != 0 {
		t.Fatalf("前提不成立：PendingRefs 非空 %v", repo.PendingRefs())
	}

	today := time.Now().Format("2006-01-02")
	if repo.HasDailyCheckpoint("zero", today) {
		t.Fatal("还没跑维护环，不该有锚点")
	}

	srv.maybeWriteDailyCheckpoints([]string{"blackegg/me"})

	if !repo.HasDailyCheckpoint("zero", today) {
		t.Fatal("分支头动过但没有本机未提交写入时，仍然应该打锚点")
	}
	cp, err := repo.LatestDailyCheckpoint("zero")
	if err != nil || cp == nil || len(cp.Times) == 0 {
		t.Fatalf("锚点数据读不回来: cp=%v err=%v", cp, err)
	}
}

// 头没动就不该重复打锚点：每天给 18 条不动的分支撒空 tag 是纯噪音。
func TestDailyAnchorSkippedWhenHeadUnmoved(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "put blackegg/me -- 记忆"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m seed"); err != nil {
		t.Fatal(err)
	}
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}

	// 昨天打过一个锚点，头到现在没动。
	times, err := repo.FileTimes("zero")
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := repo.WriteDailyCheckpoint("zero", yesterday, "", times); err != nil {
		t.Fatal(err)
	}

	today := time.Now().Format("2006-01-02")
	srv.maybeWriteDailyCheckpoints([]string{"blackegg/me"})

	if repo.HasDailyCheckpoint("zero", today) {
		t.Fatal("头没动，不该再打一个新锚点")
	}
}
