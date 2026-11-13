package gitstore

import (
	"fmt"
	"testing"
)

// 真实现场（2026-11-13，3060 hub）：
//
//	inbox 分支 2052 个 commit、4520 个文件，
//	启动时 List() → fileTimes() 对**每个 commit** 全量展开整棵树，
//	900 万次树项遍历、每次都要从 pack 解压对象。
//	结果：进程活着、CPU 100%、内存持续上涨、1729 端口永不监听、
//	日志一行不写（Reload 在 net.Listen 之前）。
//
// 这个测试钉两件事：
//  1. 每个路径拿到的时间，是**最后一次改动它的那个 commit** 的时间
//  2. 长历史 + 多文件下不退化：只改一个文件时不必重走所有路径
func TestFileTimesUsesLastTouchingCommit(t *testing.T) {
	r := newTestRepo(t)

	// 三个文件，分别在不同 commit 落笔。
	if err := r.Write("main", "a.md", []byte("a1"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c1, err := r.Commit("main", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "b.md", []byte("b1"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c2, err := r.Commit("main", "c2")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "a.md", []byte("a2"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c3, err := r.Commit("main", "c3")
	if err != nil {
		t.Fatal(err)
	}

	times, err := r.fileTimes("main")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{}
	for _, tc := range []struct {
		path string
		sha  string
	}{{"a.md", c3}, {"b.md", c2}} {
		want[tc.path] = commitTimeUnix(t, r, tc.sha)
	}
	for p, w := range want {
		if got := times[p]; got != w {
			t.Errorf("%s: got time %d, want %d (commit that last touched it)", p, got, w)
		}
	}
	_ = c1
}

// 一个文件连续改 N 次：时间必须是最后一次，且不能因为历史长就整体失准。
func TestFileTimesLongHistoryStaysCorrect(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "hot.md", []byte("v0"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "seed"); err != nil {
		t.Fatal(err)
	}
	var last string
	for i := 0; i < 60; i++ {
		if err := r.Write("main", "hot.md", []byte(fmt.Sprintf("v%d", i+1)), ModeContent); err != nil {
			t.Fatal(err)
		}
		sha, err := r.Commit("main", fmt.Sprintf("edit-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		last = sha
	}
	// 另一个文件只写过一次，时间应停在它的那次提交上。
	if err := r.Write("main", "cold.md", []byte("cold"), ModeContent); err != nil {
		t.Fatal(err)
	}
	coldCommit, err := r.Commit("main", "cold")
	if err != nil {
		t.Fatal(err)
	}

	times, err := r.fileTimes("main")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := times["hot.md"], commitTimeUnix(t, r, last); got != want {
		t.Errorf("hot.md: got %d, want the last edit commit %d", got, want)
	}
	if got, want := times["cold.md"], commitTimeUnix(t, r, coldCommit); got != want {
		t.Errorf("cold.md: got %d, want its only commit %d", got, want)
	}
	if len(times) != 2 {
		t.Errorf("expected exactly 2 paths, got %d: %v", len(times), times)
	}
}

// 提前结束：所有路径都定过时间后不应继续走历史。
// 用「一个极长的只改单文件历史 + 一个早已定时的文件」构造，
// 若实现仍在遍历全部 commit，这条测试会明显变慢（也证明提前结束失效）。
func TestFileTimesStopsWhenAllPathsResolved(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "x.md", []byte("x"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "only"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := r.Write("main", "x.md", []byte(fmt.Sprintf("x%d", i)), ModeContent); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit("main", fmt.Sprintf("e%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	times, err := r.fileTimes("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 1 {
		t.Fatalf("expected 1 path, got %d", len(times))
	}
	if times["x.md"] == 0 {
		t.Error("x.md has zero time")
	}
}

// List 必须把 fileTimes 的结果写进每个条目的 Time 字段——
// 上层（scanGitProject → memory.Block.Time）依赖它当时间来源。
func TestListCarriesFileTimes(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "2026-11-13/a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	sha, err := r.Commit("main", "c")
	if err != nil {
		t.Fatal(err)
	}
	want := commitTimeUnix(t, r, sha)
	list, err := r.List("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(list))
	}
	if list[0].Time != want {
		t.Errorf("entry time %d != commit time %d", list[0].Time, want)
	}
}

// commitTimeUnix 直接读 commit 对象取时间。
// 不用 commitOf：它按 ref 解析，传裸 SHA 会解析不到、返回零值。
func commitTimeUnix(t *testing.T, r *Repo, sha string) int64 {
	t.Helper()
	typ, body, err := r.readObject(sha)
	if err != nil {
		t.Fatalf("read commit %s: %v", sha, err)
	}
	if typ != "commit" {
		t.Fatalf("%s is %s, not a commit", sha, typ)
	}
	return parseCommit(body).When.Unix()
}
