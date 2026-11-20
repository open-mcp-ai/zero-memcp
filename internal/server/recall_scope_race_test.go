package server

import (
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 带 scope 检索落空时，那条「范围外有」的兜底判定必须在锁内完成。
//
// 2026-11-16 线上事故：hub 被 runtime 直接杀掉——
//
//	fatal error: concurrent map iteration and map write
//	internal/index.(*Index).SearchCovered  index.go:202
//	  ← server.(*Server).execStatement     server.go:1733
//
// 根因：该兜底路径写在 s.mu.RUnlock() 之后，SearchCovered 遍历索引 map 时
// 并发写入正在改同一批 map。这不是 error，是 fatal，进程没了。
//
// 这条测试构造「带 scope 且必然落空」的查询（落空才会走那个兜底分支），
// 同时并发写入让索引 map 持续变动，用 -race / map 迭代检查把它逼出来。
//
// 注意：它必须跑在 -race 下才有稳定信号（make test 已强开 -race）。
func TestScopedRecallMissDoesNotRaceWithWrites(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 先放一篇内容，让索引里有底子。
	if _, err := callMCPVars(t, srv, tok, "blackegg/me@fact:seed.md", "alpha beta gamma"); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写者：持续改动索引 map（AddWithTime / Remove）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			body := "alpha beta gamma delta epsilon " + itoa(i)
			if _, err := callMCPVars(t, srv, tok, "blackegg/me@fact:churn"+itoa(i%8)+".md", body); err == nil {
				_, _ = callMCP(t, srv, tok, "commit blackegg/me@fact -m churn")
			}
		}
	}()

	// 读者：反复做「带 scope 且必然落空」的 recall，逼它走那条兜底分支。
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			// zzzznotfound 保证零命中；scope 存在保证进入兜底判定。
			_, _ = callMCP(t, srv, tok, "recall zzzznotfound --scope blackegg/me@fact")
		}
	}()

	time.Sleep(3 * time.Second)
	close(stop)
	wg.Wait()
}
