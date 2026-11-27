package index

import (
	"fmt"
	"testing"
)

// 权限过滤必须在**排序之后**执行，不能放在 posting 扫描的内层。
//
// 这条测试钉住的是成本形状，不是"能跑"：早期实现把 filter 放在
// `for _, e := range entries` 里，于是查询词在索引里出现的**每一条
// posting** 都要跑一次完整的权限链（SplitID → branchVisible →
// roleAllowsPath → allowed → 挂载表 → ACL）。实测在 6.4 万块的库上：
//
//	recall 的   → 9.5~11.2s
//	recall 记忆 → 2.2~9.2s
//	recall <生僻词，只命中 1 篇> → 2.5~3.4s
//
// 最后一条是决定性的：命中数只有 1，耗时却和热门词同量级——说明成本
// 在"扫了多少 posting"，不在"返回多少结果"。
func TestFilterRunsAfterRankingNotPerPosting(t *testing.T) {
	ix := New()
	const n = 4000
	// 全部文档含同一个词，于是这个词的 posting 列表有 n 条。
	for i := 0; i < n; i++ {
		ix.Add(fmt.Sprintf("p@b/doc%04d", i), "海马体 与 记忆体 的设计取舍")
	}
	// 只允许偶数号文档，模拟"受限凭证只看得到一部分分支"。
	allowed := func(addr string) bool {
		var i int
		if _, err := fmt.Sscanf(addr, "p@b/doc%d", &i); err != nil {
			return false
		}
		return i%2 == 0
	}

	calls := 0
	counting := func(addr string) bool {
		calls++
		return allowed(addr)
	}

	const limit = 3
	hits := ix.SearchCovered("海马体", limit, 0, 0, nil, counting)

	if len(hits) != limit {
		t.Fatalf("expected %d hits, got %d", limit, len(hits))
	}
	for _, h := range hits {
		if !allowed(h.Addr) {
			t.Fatalf("hit %q was not allowed by the filter", h.Addr)
		}
	}

	// 关键断言：filter 的调用次数必须远小于 posting 总数。
	// 允许的松弛度：分数并列时可能多看几条。旧实现是 4000 次（=n）。
	const maxCalls = 50
	if calls > maxCalls {
		t.Fatalf("filter called %d times for %d postings (limit=%d); "+
			"it is being applied inside the posting scan instead of after ranking",
			calls, n, limit)
	}
	if n <= maxCalls {
		t.Fatalf("test corpus too small to prove anything: n=%d", n)
	}
}

// 移到排序之后不能改变结果：同一个 filter 下，
// "先全量算分再按序过滤"与参照实现（自己逐条过滤后排序）必须一致。
func TestFilterAfterRankingMatchesReference(t *testing.T) {
	ix := New()
	// 分数各不相同：用不同的词频制造排序差异。
	for i := 0; i < 200; i++ {
		text := "记忆体"
		for j := 0; j <= i%7; j++ {
			text += " 海马体"
		}
		ix.Add(fmt.Sprintf("p@b/d%03d", i), text)
	}
	// 过滤掉一部分，且被过滤的包含高分项——这正是"过滤顺序"最容易翻车的地方。
	disallow := map[string]bool{}
	for i := 0; i < 200; i += 3 {
		disallow[fmt.Sprintf("p@b/d%03d", i)] = true
	}
	filter := func(addr string) bool { return !disallow[addr] }

	got := ix.SearchCovered("海马体", 5, 0, 0, nil, filter)

	// 参照：不过滤取全部，再按同样规则手工筛。
	want := ix.SearchCovered("海马体", 0, 0, 0, nil, nil)
	ref := make([]string, 0, 5)
	for _, h := range want {
		if filter(h.Addr) {
			ref = append(ref, h.Addr)
			if len(ref) == 5 {
				break
			}
		}
	}
	if len(got) != len(ref) {
		t.Fatalf("hit count differs: got %d, reference %d", len(got), len(ref))
	}
	for i := range ref {
		if got[i].Addr != ref[i] {
			t.Fatalf("rank %d differs: got %q, reference %q", i, got[i].Addr, ref[i])
		}
	}
}

// 过滤掉的条目不能挤掉有权限的条目——即 limit 必须**在过滤之后**才算满。
//
// 这是把 filter 移出扫描循环时最需要守住的语义：如果先截断再过滤，
// 受限凭证的 limit=3 会因为前 3 条都被过滤而返回空，看起来像"搜不到"。
func TestFilteredOutHitsDoNotConsumeLimit(t *testing.T) {
	ix := New()
	// 让 d00..d04 分数最高但全部无权，d05..d09 分数次之且有权。
	for i := 0; i < 10; i++ {
		text := "记忆体"
		for j := 0; j < 10-i; j++ { // i 越小词频越高
			text += " 海马体"
		}
		ix.Add(fmt.Sprintf("p@b/d%02d", i), text)
	}
	filter := func(addr string) bool {
		var i int
		fmt.Sscanf(addr, "p@b/d%d", &i)
		return i >= 5
	}

	hits := ix.SearchCovered("海马体", 3, 0, 0, nil, filter)
	if len(hits) != 3 {
		t.Fatalf("expected 3 allowed hits, got %d (filtered-out hits consumed the limit)", len(hits))
	}
	for _, h := range hits {
		if !filter(h.Addr) {
			t.Fatalf("hit %q is not allowed", h.Addr)
		}
	}
}
