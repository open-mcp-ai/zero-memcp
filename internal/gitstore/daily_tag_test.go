package gitstore

import (
	"strings"
	"testing"
	"time"
)

// 按日锚点 tag 必须随 git 走：换一个机器（没有本地缓存，只有 tag 与对象），
// 同样能借 tag 截断父链，不需要重走历史。
//
// 理由（用户定调）：每天新写入的时候打个新 tag，然后召回的时候
// 可以到 tag 为止。它就相当于新开一个分支的感觉。
func TestDailyCheckpointTagPortability(t *testing.T) {
	r := newTestRepo(t)
	tick := func() { time.Sleep(1050 * time.Millisecond) }

	for i := 0; i < 5; i++ {
		if err := r.Write("main", "a.md", []byte{byte('a' + i)}, ModeContent); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit("main", "c"); err != nil {
			t.Fatal(err)
		}
		tick()
	}

	// 第一次：写锚点 tag
	summary := "今天完成了 5 次提交"
	allTimes, err := r.fileTimes("main")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteDailyCheckpoint("main", "2026-11-17", summary, allTimes); err != nil {
		t.Fatal(err)
	}

	// 验证 tag 真的在 refs/tags/zmcp/main/2026-11-17 下
	tagName := "zmcp/main/2026-11-17"
	tagObj, ok, err := r.ReadTagObject(tagName)
	if err != nil || !ok {
		t.Fatalf("tag 没读到: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(tagObj.Body, summary) {
		t.Fatalf("tag 正文没有摘要：%q", tagObj.Body)
	}
	if !strings.Contains(tagObj.Body, "zmcp-checkpoint: ") {
		t.Fatalf("tag 正文没有指向数据的指针：%q", tagObj.Body)
	}

	// 模拟「换了台机器，本地没有缓存文件」：
	// 把 .git/zmcp/ 删掉，只留 tag 与 objects
	_ = r.FS().RemoveAll(filepathJoin(r.Path, "zmcp"))

	// 此时 LatestDailyCheckpoint 必须仍能从 tag 读回数据
	cp, err := r.LatestDailyCheckpoint("main")
	if err != nil || cp == nil {
		t.Fatalf("从 tag 读回数据失败: cp=%v err=%v", cp, err)
	}
	if len(cp.Times) == 0 {
		t.Fatal("读回的数据是空的")
	}

	// 再写一个新提交：fileTimes 必须能借这个 tag 截断，
	// 结果必须包含旧文件的时间（来自 tag）和新文件的时间（来自新提交）
	if err := r.Write("main", "b.md", []byte("b"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c-new"); err != nil {
		t.Fatal(err)
	}
	timesAfter, err := r.fileTimes("main")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := timesAfter["a.md"]; !ok {
		t.Fatal("tag 里的旧文件时间丢了")
	}
	if _, ok := timesAfter["b.md"]; !ok {
		t.Fatal("新提交的文件时间没拿到")
	}
}

func filepathJoin(elem ...string) string {
	var s string
	for i, e := range elem {
		if i == 0 {
			s = e
		} else {
			s += "/" + e
		}
	}
	return s
}
