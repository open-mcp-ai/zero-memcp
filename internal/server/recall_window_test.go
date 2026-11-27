package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// recall 的时间窗：--before / --older-than 必须真的缩小候选集。
//
// 这条功能的动机是 Issue #13 的另一面：本会话刚聊过的内容已经在
// 上下文里，召回它等于把手里已有的东西再读一遍。降权是"查出来再压分"，
// 正确方向是根本不进候选集——所以这里钉的是"查不到"，不是"分变低"。
func TestRecallOlderThanExcludesRecentSlices(t *testing.T) {
	s, _ := newTestServer(t, `
[server]
data = "."

[[project]]
name = "notes"
owner = "alice"
mode = "0755"

[[user]]
name = "alice"
root = true
`)

	// 两条内容相近的切片，一条时间设成 10 天前，一条设成现在。
	// 索引的时间来自 AddWithTime，所以这里直接按地址构造。
	old := "notes@main/old.md"
	fresh := "notes@main/fresh.md"
	body := "海马体负责把短期记忆转成长期记忆"
	s.idxNow().AddWithTime(old, body, time.Now().Add(-10*24*time.Hour).Unix())
	s.idxNow().AddWithTime(fresh, body, time.Now().Unix())

	// 不限定：两条都在
	all := s.idxNow().Search("海马体", 10, 0)
	if len(all) != 2 {
		t.Fatalf("前提不成立：不限定时应命中 2 条，实际 %d 条", len(all))
	}

	// --older-than 1h：只剩 10 天前那条
	var got []string
	for _, h := range all {
		if (timeWindow{before: time.Now().Add(-time.Hour).Unix() + 1}).keep(s.idxNow().DocTime(h.Addr)) {
			got = append(got, h.Addr)
		}
	}
	if len(got) != 1 || got[0] != old {
		t.Fatalf("--older-than 1h 应只剩 %s，实际 %v", old, got)
	}
}

// 解析必须严格：写错的时间不该被静默忽略。
//
// 静默忽略比报错贵得多——调用方以为限定过范围，实际查了全库，
// 于是把「全库最近的东西」当成「三天前的历史」，错的结论一路传下去。
func TestParseTimeWindowRejectsBadInput(t *testing.T) {
	now := time.Date(2026, 11, 27, 21, 0, 0, 0, time.Local)

	bad := []string{
		"query --before",
		"query --older-than",
		"query --older-than 2x",
		"query --older-than abc",
		"query --older-than -3h",
		"query --before not-a-date",
	}
	for _, in := range bad {
		if _, _, err := parseTimeWindow(in, now); err == nil {
			t.Errorf("%q 应报错，实际被接受了", in)
		}
	}

	good := []struct {
		in     string
		query  string
		before int64
		after  int64
	}{
		{"海马体", "海马体", 0, 0},
		{"海马体 --older-than 2h", "海马体", now.Add(-2 * time.Hour).Unix() + 1, 0},
		{"海马体 --older-than 3d", "海马体", now.Add(-72 * time.Hour).Unix() + 1, 0},
		{"海马体 --before 2026-11-20", "海马体",
			time.Date(2026, 11, 20, 0, 0, 0, 0, time.Local).Unix(), 0},
		{"海马体 --after 2026-11-20", "海马体", 0,
			time.Date(2026, 11, 20, 0, 0, 0, 0, time.Local).Unix()},
	}
	for _, c := range good {
		q, w, err := parseTimeWindow(c.in, now)
		if err != nil {
			t.Errorf("%q 不该报错: %v", c.in, err)
			continue
		}
		if q != c.query {
			t.Errorf("%q 的查询词应为 %q，实际 %q", c.in, c.query, q)
		}
		if w.before != c.before || w.after != c.after {
			t.Errorf("%q 的窗口应为 before=%d after=%d，实际 before=%d after=%d",
				c.in, c.before, c.after, w.before, w.after)
		}
	}
}

// 时间未知的块必须保留。
//
// 拿不到时间只能承认"不知道"；把它当成"太新"删掉，等于让缺元数据的
// 旧数据从检索里静默消失——那比"多查一条近期内容"严重得多。
func TestTimeWindowKeepsUnknownTimestamps(t *testing.T) {
	w := timeWindow{before: time.Now().Unix(), after: time.Now().Add(-24 * time.Hour).Unix()}
	if !w.keep(0) {
		t.Fatal("时间未知的块不该被时间窗筛掉")
	}
}

// 端到端：动词层必须真的把 --older-than 用在检索上，而不是只解析出来。
//
// 这条是防"解析了但没接线"——那类 bug 从输出上完全看不出来，
// 因为不带 flag 时行为一模一样。
func TestRecallVerbAppliesOlderThan(t *testing.T) {
	s, _ := newTestServer(t, `
[server]
data = "."

[[project]]
name = "notes"
owner = "alice"
mode = "0755"

[[user]]
name = "alice"
root = true
`)

	// 写一条，然后把它的索引时间改成 10 天前。
	out, err := s.ExecLineVars(context.Background(), "put $1", nil, []string{"海马体负责记忆巩固"})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	addr := strings.TrimPrefix(strings.TrimSpace(out), "ok ")
	addr = strings.Fields(addr)[0]
	id := strings.Replace(addr, "notes/", "notes@"+s.defaultBranch+"/", 1)
	s.idxNow().AddWithTime(id, "海马体负责记忆巩固", time.Now().Add(-10*24*time.Hour).Unix())

	// 不限定 → 能查到
	if got, err := s.ExecLine(context.Background(), "recall 海马体"); err != nil || !strings.Contains(got, "海马体") {
		t.Fatalf("不限定时间窗时应能查到，实际 err=%v out=%q", err, got)
	}
	// --older-than 1h → 仍然能查到（它是 10 天前的）
	if got, err := s.ExecLine(context.Background(), "recall 海马体 --older-than 1h"); err != nil {
		t.Fatalf("--older-than 1h 报错: %v", err)
	} else if !strings.Contains(got, "海马体") {
		t.Fatalf("10 天前的切片不该被 --older-than 1h 筛掉，实际: %q", got)
	}
	// --older-than 30d → 该被筛掉
	got, err := s.ExecLine(context.Background(), "recall 海马体 --older-than 30d")
	if err != nil {
		t.Fatalf("--older-than 30d 报错: %v", err)
	}
	if strings.Contains(got, "海马体负责记忆巩固") {
		t.Fatalf("10 天前的切片不该出现在 --older-than 30d 的结果里: %q", got)
	}
	// 坏参数必须报错，不能静默当成没给
	if _, err := s.ExecLine(context.Background(), "recall 海马体 --older-than 2x"); err == nil {
		t.Fatal("--older-than 2x 应报错")
	}
}
