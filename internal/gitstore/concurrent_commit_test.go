package gitstore

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// 同一进程内，多个 goroutine 并发写同一条分支并提交，一条都不能丢。
func TestStressConcurrentCommitSameBranch(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "seed.md", []byte("seed\n"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}

	const N, W = 40, 8
	var wg sync.WaitGroup
	for w := 0; w < W; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < N; i++ {
				p := fmt.Sprintf("log/w%d-%d.md", w, i)
				if err := r.Write("zero", p, []byte(fmt.Sprintf("writer %d item %d\n", w, i)), ModeContent); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				if _, err := r.Commit("zero", fmt.Sprintf("w%d #%d", w, i)); err != nil {
					t.Errorf("commit: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	entries, err := r.List("zero")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Path] = true
	}
	var missing []string
	for w := 0; w < W; w++ {
		for i := 0; i < N; i++ {
			if !got[fmt.Sprintf("log/w%d-%d.md", w, i)] {
				missing = append(missing, fmt.Sprintf("w%d-%d", w, i))
			}
		}
	}
	t.Logf("应得 %d 篇，实际 %d 篇，丢 %d 篇", N*W, len(got)-1, len(missing))
	if len(missing) > 0 {
		head := strings.Join(missing, " ")
		if len(head) > 300 {
			head = head[:300]
		}
		t.Fatalf("并发写同分支丢了 %d 篇，例如：%s", len(missing), head)
	}
}

// 不同分支并发提交必须互相不阻塞，且各自内容完整。
func TestConcurrentCommitDifferentBranches(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	const N = 30
	branches := []string{"a", "b", "c", "d"}
	var wg sync.WaitGroup
	for _, br := range branches {
		wg.Add(1)
		go func(br string) {
			defer wg.Done()
			for i := 0; i < N; i++ {
				p := fmt.Sprintf("%s-%d.md", br, i)
				if err := r.Write(br, p, []byte(br), ModeContent); err != nil {
					t.Errorf("write %s: %v", br, err)
					return
				}
				if _, err := r.Commit(br, "x"); err != nil {
					t.Errorf("commit %s: %v", br, err)
					return
				}
			}
		}(br)
	}
	wg.Wait()

	for _, br := range branches {
		entries, err := r.List(br)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, e := range entries {
			got[e.Path] = true
		}
		for i := 0; i < N; i++ {
			if !got[fmt.Sprintf("%s-%d.md", br, i)] {
				t.Fatalf("分支 %s 丢了第 %d 篇", br, i)
			}
		}
		// 也不能串味：别的分支的文件不该出现在这里
		for _, other := range branches {
			if other == br {
				continue
			}
			if got[fmt.Sprintf("%s-0.md", other)] {
				t.Fatalf("分支 %s 混入了分支 %s 的文件", br, other)
			}
		}
	}
}

// 提交排队有超时：拿不到令牌必须报错，不能永久挂住。
func TestCommitTokenTimesOut(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "a.md", []byte("x"), ModeContent); err != nil {
		t.Fatal(err)
	}

	// 手工占住令牌，模拟「上一个提交卡死」
	release, err := r.acquireCommitToken("zero")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		_, cerr := r.Commit("zero", "blocked")
		done <- cerr
	}()

	select {
	case err := <-done:
		t.Fatalf("第二个提交不该立刻返回：%v", err)
	case <-time.After(200 * time.Millisecond):
		// 还在排队，符合预期
	}

	// 释放之后应当能立刻完成
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("令牌释放后提交应成功: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("令牌释放后提交仍未完成")
	}
}
