package server

import (
	"sync"
	"testing"
	"time"
)

// 两条不同分支的写者必须能同时进行——这是「容纳更多 agent」的底线。
func TestDifferentBranchesWriteInParallel(t *testing.T) {
	s := &Server{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	done := make(chan string, 2)

	for _, br := range []string{"main", "main.record"} {
		wg.Add(1)
		go func(branch string) {
			defer wg.Done()
			release, err := s.acquireBranchWrite("proj", branch)
			if err != nil {
				done <- "err:" + branch
				return
			}
			<-start
			// 模拟一次重建耗时
			time.Sleep(200 * time.Millisecond)
			release()
			done <- branch
		}(br)
	}

	time.Sleep(50 * time.Millisecond) // 让两个 goroutine 都拿到令牌
	close(start)
	t0 := time.Now()
	wg.Wait()
	elapsed := time.Since(t0)

	if elapsed > 400*time.Millisecond {
		t.Fatalf("两条不同分支没能并行：耗时 %v（串行应约 400ms，并行应约 200ms）", elapsed)
	}
}

// 同一条分支的两个写者必须排队——它们改的是同一棵树。
func TestSameBranchWritersSerialize(t *testing.T) {
	s := &Server{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.acquireBranchWrite("proj", "main")
			if err != nil {
				return
			}
			defer release()
			<-start
			time.Sleep(150 * time.Millisecond)
		}()
	}
	time.Sleep(80 * time.Millisecond)
	close(start)
	t0 := time.Now()
	wg.Wait()
	if elapsed := time.Since(t0); elapsed < 250*time.Millisecond {
		t.Fatalf("同分支两个写者没有排队：耗时 %v（应 ≥300ms）", elapsed)
	}
}
