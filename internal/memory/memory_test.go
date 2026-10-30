package memory

import "testing"

// 单层和多层项目名必须一视同仁。
func TestSplitAddrWithProjects(t *testing.T) {
	projects := []string{"me", "daily", "blackegg/me", "blackegg/daily", "a/b/c"}

	cases := []struct {
		in      string
		project string
		path    string
		ok      bool
	}{
		// 多层：按最长前缀匹配
		{"blackegg/daily/2026-10-30/x.md", "blackegg/daily", "2026-10-30/x.md", true},
		{"blackegg/me/01.md", "blackegg/me", "01.md", true},
		{"a/b/c/deep/x.md", "a/b/c", "deep/x.md", true},
		// 单层：同样匹配
		{"daily/2026-10-30/x.md", "daily", "2026-10-30/x.md", true},
		{"me/01-shape.md", "me", "01-shape.md", true},
		// 前导斜杠容错
		{"/blackegg/me/01.md", "blackegg/me", "01.md", true},
		// 空与非法
		{"", "", "", false},
		{"unknown/x.md", "", "", false}, // 不在候选里就拒绝，不猜
	}

	for _, c := range cases {
		p, path, ok := SplitAddrWithProjects(c.in, projects)
		if ok != c.ok || p != c.project || path != c.path {
			t.Errorf("SplitAddrWithProjects(%q) = (%q, %q, %v)，要 (%q, %q, %v)",
				c.in, p, path, ok, c.project, c.path, c.ok)
		}
	}
}

// 最长前缀优先：同时存在 me 和 blackegg/me 时不能匹配错。
func TestSplitAddrLongestPrefixWins(t *testing.T) {
	projects := []string{"me", "blackegg/me"}
	p, path, ok := SplitAddrWithProjects("blackegg/me/01.md", projects)
	if !ok || p != "blackegg/me" || path != "01.md" {
		t.Fatalf("应匹配最长的 blackegg/me，得到 (%q, %q, %v)", p, path, ok)
	}
}

func TestProjectOf(t *testing.T) {
	if got := ProjectOf("daily/2026-10-30/x.md"); got != "daily" {
		t.Errorf("要 daily，得到 %q", got)
	}
	if got := ProjectOf("bare"); got != "bare" {
		t.Errorf("单段应原样返回，得到 %q", got)
	}
}

// 地址永远带项目前缀——这是权限判定的单位。
func TestBlockAddr(t *testing.T) {
	b := Block{Project: "blackegg/daily", Path: "2026-10-30/x.md"}
	if got := b.Addr(); got != "blackegg/daily/2026-10-30/x.md" {
		t.Errorf("要 blackegg/daily/2026-10-30/x.md，得到 %q", got)
	}
}

// Find 要同时接受带前缀和不带前缀两种写法。
func TestTreeFind(t *testing.T) {
	tr := &Tree{
		Project: "daily",
		Blocks: []Block{
			{Project: "daily", Path: "a.md"},
			{Project: "daily", Path: "sub/b.md"},
		},
	}
	for _, addr := range []string{"a.md", "daily/a.md", "/daily/a.md"} {
		if _, ok := tr.Find(addr); !ok {
			t.Errorf("Find(%q) 应命中", addr)
		}
	}
	if _, ok := tr.Find("nope.md"); ok {
		t.Error("不存在的路径不该命中")
	}
}

// 规则位是唯一的注入判据。
func TestInjectable(t *testing.T) {
	tr := &Tree{
		Project: "me",
		Blocks: []Block{
			{Project: "me", Path: "rules/01.md", Rule: true},
			{Project: "me", Path: "notes/01.md", Rule: false},
		},
	}
	inj := tr.Injectable()
	if len(inj) != 1 || inj[0].Path != "rules/01.md" {
		t.Fatalf("只应有 1 条规则块，得到 %+v", inj)
	}
	// 全部块都进索引：规则也需要能被 recall 找到，
	// 否则「我知道有这条规则但找不到」会成为常态。
	if got := len(tr.Indexable()); got != 2 {
		t.Fatalf("所有块都应可索引，得到 %d", got)
	}
}

func TestTreeSort(t *testing.T) {
	tr := &Tree{
		Project: "x",
		Blocks: []Block{
			{Path: "c.md"},
			{Path: "a.md"},
			{Path: "b.md"},
		},
	}
	tr.Sort()
	for i, want := range []string{"a.md", "b.md", "c.md"} {
		if tr.Blocks[i].Path != want {
			t.Fatalf("第 %d 个应为 %s，得到 %s", i, want, tr.Blocks[i].Path)
		}
	}
}

func TestStats(t *testing.T) {
	tr := &Tree{
		Blocks: []Block{
			{Rule: true, Size: 10},
			{Rule: false, Size: 20},
		},
	}
	s := tr.Stats()
	if s.Blocks != 2 || s.Rules != 1 || s.IndexBytes != 30 {
		t.Fatalf("统计不对: %+v", s)
	}
}
