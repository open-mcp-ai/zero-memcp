package server

import (
	"strings"
	"testing"
	"time"
)

// 流言止于智者：ref 没有真正推进就不该扇出。
//
// 重推一个已存在的提交是很常见的（重试、脚本重复执行）。那种情况下
// ref 不变，扇出一轮 fetch/merge/push 只是白跑一个完整网络往返。
func TestAdvancedRefsOnlyReportsRealProgress(t *testing.T) {
	before := map[string]string{"main": "aaa", "main.record": "bbb"}

	cases := []struct {
		name  string
		after map[string]string
		want  []string
	}{
		{"什么都没变", map[string]string{"main": "aaa", "main.record": "bbb"}, nil},
		{"一个分支前进", map[string]string{"main": "ccc", "main.record": "bbb"}, []string{"main"}},
		{"两个分支都前进", map[string]string{"main": "ccc", "main.record": "ddd"}, []string{"main", "main.record"}},
		{"新分支出现", map[string]string{"main": "aaa", "main.record": "bbb", "topic": "eee"}, []string{"topic"}},
		// 删除不算推进：没有新消息可传播，而记忆历史本来就只追加。
		{"分支被删", map[string]string{"main": "aaa"}, nil},
	}
	for _, c := range cases {
		got := advancedRefs(before, c.after)
		var names []string
		for _, g := range got {
			names = append(names, g.Branch)
		}
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: advancedRefs = %v，期望 %v", c.name, names, c.want)
		}
	}
}

// 扇出必须有可观测的进出状态：完成 / 进行中 / 失败都要能看见。
func TestFanoutHubTracksPerTargetState(t *testing.T) {
	h := newFanoutHub()
	h.add(&FanoutBatch{
		ID:        "b1",
		Project:   "p",
		Ref:       "main.record",
		NewSHA:    "2ae6c6ef2a11",
		Trigger:   "received push",
		StartedAt: time.Now(),
		Targets: map[string]FanoutTarget{
			"peer-a": {Status: "pending", At: time.Now()},
			"peer-b": {Status: "pending", At: time.Now()},
		},
	})

	if lines := h.describeLines(); len(lines) == 0 || !strings.Contains(lines[0], "broadcasting") {
		t.Fatalf("刚创建的批次应报告 broadcasting，得到 %v", lines)
	}

	h.updateTarget("b1", "peer-a", "done", "")
	if h.batches[0].Completed {
		t.Fatal("还有一个目标未终态时不该标完成")
	}

	h.updateTarget("b1", "peer-b", "failed", "connection refused")
	if !h.batches[0].Completed {
		t.Fatal("所有目标都终态后应标完成")
	}
	if h.batches[0].DoneAt.IsZero() {
		t.Fatal("完成时应记录结束时刻")
	}

	lines := h.describeLines()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "completed") {
		t.Errorf("完成后状态应显示 completed:\n%s", joined)
	}
	if !strings.Contains(joined, "connection refused") {
		t.Errorf("失败原因必须可见，否则「未完成」等于没说:\n%s", joined)
	}
	if !strings.Contains(joined, "peer-a: done") {
		t.Errorf("每个目标的结局都要列出:\n%s", joined)
	}
}

// 只有配了 on_receive 的远端参与扇出；没配的不该被触发。
func TestOnReceiveTargetsRespectsFlag(t *testing.T) {
	srv, _ := newTestServer(t, "")
	if got := srv.onReceiveTargets("nope"); len(got) != 0 {
		t.Fatalf("没有远端时应为空，得到 %v", got)
	}
}

// 批次历史只保留最近若干条，避免 status 无限增长。
func TestFanoutHubKeepsBoundedHistory(t *testing.T) {
	h := newFanoutHub()
	for i := 0; i < 10; i++ {
		h.add(&FanoutBatch{ID: string(rune('a' + i)), StartedAt: time.Now()})
	}
	h.mu.Lock()
	n := len(h.batches)
	h.mu.Unlock()
	if n > 5 {
		t.Fatalf("批次历史应上限 5 条，实际 %d", n)
	}
}
