package gitstore

import (
	"testing"
	"time"
)

// 锚点只许加速，不许改变答案。
//
// 这是这条优化的唯一风险点：如果锚点里的时间和「真走一遍父链」
// 算出来的不一致，recency boost 会静默偏掉——文件还在、时间也在，
// 只是答案是错的，没有任何报错。
//
// 对拍：同一份历史，一次有锚点、一次没有，结果必须逐条相同。
func TestCheckpointDoesNotChangeAnswer(t *testing.T) {
	r := newTestRepo(t)
	tick := func() { time.Sleep(1050 * time.Millisecond) }

	// 三次跨秒提交，让每个文件的最后改动时间各不相同
	if err := r.Write("main", "a.md", []byte("a1"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c1"); err != nil {
		t.Fatal(err)
	}
	tick()

	if err := r.Write("main", "b.md", []byte("b1"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c2"); err != nil {
		t.Fatal(err)
	}
	tick()

	if err := r.Write("main", "a.md", []byte("a2"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c3"); err != nil {
		t.Fatal(err)
	}

	// 无锚点：真走一遍父链
	if err := r.FS().Remove(r.checkpointPath("main")); err != nil && !isNotExist(err) {
		t.Fatal(err)
	}
	want, err := r.fileTimes("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 {
		t.Fatalf("应有 2 条路径，实际 %d", len(want))
	}

	// 上面那次调用会写锚点；再算一次，这次会命中锚点
	if _, err := r.ReadCheckpoint("main"); err != nil {
		t.Fatal(err)
	}
	got, err := r.fileTimes("main")
	if err != nil {
		t.Fatal(err)
	}

	for p, w := range want {
		if got[p] != w {
			t.Fatalf("%s: 命中锚点后时间变了（%d → %d）——锚点只许加速，不许改变答案",
				p, w, got[p])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("路径数变了：%d → %d", len(want), len(got))
	}
}

// 锚点必须真的截断遍历：追加新提交后，只需要走「锚点之后」那一段。
func TestCheckpointShortensTraversal(t *testing.T) {
	r := newTestRepo(t)
	for i := 0; i < 5; i++ {
		if err := r.Write("main", "f.md", []byte{byte('a' + i)}, ModeContent); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit("main", "c"); err != nil {
			t.Fatal(err)
		}
	}
	// 第一次：建立锚点
	if _, err := r.fileTimes("main"); err != nil {
		t.Fatal(err)
	}
	cp, err := r.ReadCheckpoint("main")
	if err != nil || cp == nil {
		t.Fatalf("第一次调用后应写下锚点：cp=%v err=%v", cp, err)
	}

	// 再提交一次
	if err := r.Write("main", "g.md", []byte("g"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c-next"); err != nil {
		t.Fatal(err)
	}
	times, err := r.fileTimes("main")
	if err != nil {
		t.Fatal(err)
	}
	// 旧文件的时间必须仍然正确（来自锚点），新文件来自新提交
	if _, ok := times["f.md"]; !ok {
		t.Fatal("锚点里的旧路径丢了")
	}
	if _, ok := times["g.md"]; !ok {
		t.Fatal("新路径没有拿到时间")
	}
}
