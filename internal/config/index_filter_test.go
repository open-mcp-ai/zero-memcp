package config

import (
	"strings"
	"testing"
)

// [index] include/exclude：按分支控制索引范围。
//
// 动机是实测的：本机 ~60,800 块里 main.chat + main.record + inbox 三条
// 占 89%，而索引体积/内存/启动时间基本由它们决定。
func TestIndexBranchFilterMatching(t *testing.T) {
	cases := []struct {
		name    string
		cfg     IndexConfig
		project string
		branch  string
		want    bool
	}{
		{"无过滤：全建", IndexConfig{Enabled: true}, "BlackEggAI/zero", "main.chat", true},
		{"排除一条精确分支", IndexConfig{Enabled: true, Exclude: []string{"BlackEggAI/zero@main.chat"}}, "BlackEggAI/zero", "main.chat", false},
		{"排除不影响别的分支", IndexConfig{Enabled: true, Exclude: []string{"BlackEggAI/zero@main.chat"}}, "BlackEggAI/zero", "main.record", true},
		// `*` 必须跨 `/`：项目名自带斜杠，path.Match 在这里会永远不命中。
		{"通配跨斜杠", IndexConfig{Enabled: true, Exclude: []string{"*@main.chat"}}, "BlackEggAI/zero", "main.chat", false},
		{"通配不误伤", IndexConfig{Enabled: true, Exclude: []string{"*@main.chat"}}, "BlackEggAI/zero", "main.fact", true},
		{"按项目排除全部分支", IndexConfig{Enabled: true, Exclude: []string{"BlackEggAI/weft"}}, "BlackEggAI/weft", "main.record", false},
		{"白名单只放一条", IndexConfig{Enabled: true, Include: []string{"BlackEggAI/zero@ops"}}, "BlackEggAI/zero", "ops", true},
		{"白名单外的一律不建", IndexConfig{Enabled: true, Include: []string{"BlackEggAI/zero@ops"}}, "BlackEggAI/zero", "main.chat", false},
		{"总开关关闭时全不建", IndexConfig{Enabled: false}, "BlackEggAI/zero", "ops", false},
	}
	for _, c := range cases {
		if got := c.cfg.MatchBranch(c.project, c.branch); got != c.want {
			t.Errorf("%s: MatchBranch(%s@%s)=%v 期望 %v", c.name, c.project, c.branch, got, c.want)
		}
	}
}

// include 与 exclude 同时配置必须被拒：一个白名单加一个黑名单，
// 「哪些算在内」没有唯一答案，而这个项目不猜。
func TestIndexFilterRejectsBothLists(t *testing.T) {
	_, err := ParseFile([]byte(`
[server]
data = "/tmp/x"

[index]
enabled = true
include = ["a/b@main"]
exclude = ["a/b@other"]
`))
	if err == nil {
		t.Fatal("include 与 exclude 同时配置必须被拒")
	}
	if !strings.Contains(err.Error(), "include and exclude") {
		t.Fatalf("报错要说清是哪一处冲突，实际: %v", err)
	}
}

// 默认异步：配置驱动的实例启动即可服务。
//
// 必须测到 ParseFile 这一层：如果只测 IndexConfig 零值，ParseFile 里
// 「无条件置 asyncSet = true」的 bug 就不会被抓到——[index] 段里只写
// enabled = true 的真实配置，全都会退回同步建索引、启动卡死数分钟。
func TestIndexAsyncDefaultsOn(t *testing.T) {
	var c IndexConfig
	if !c.AsyncBuild() {
		t.Fatal("未配置 async 时应默认后台构建")
	}
	c.Async, c.asyncSet = false, true
	if c.AsyncBuild() {
		t.Fatal("显式 async = false 时必须尊重")
	}

	const base = "[[user]]\nname = \"zero\"\nroot = true\n[server]\ndata = \"/tmp/x\"\n"
	omitted, err := ParseFile([]byte(base + "[index]\nenabled = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !omitted.Index.AsyncBuild() {
		t.Fatal("ParseFile: [index] 只写 enabled = true（未写 async）时，AsyncBuild() 必须仍为 true")
	}

	explicitFalse, err := ParseFile([]byte(base + "[index]\nenabled = true\nasync = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if explicitFalse.Index.AsyncBuild() {
		t.Fatal("ParseFile: 显式写了 async = false 时必须尊重")
	}

	explicitTrue, err := ParseFile([]byte(base + "[index]\nenabled = true\nasync = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !explicitTrue.Index.AsyncBuild() {
		t.Fatal("ParseFile: 显式写了 async = true 时必须为 true")
	}

	// 同理：光写 [index]（未写 enabled）不得把 enabledSet 置真。
	emptySection, err := ParseFile([]byte(base + "[index]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if emptySection.Index.enabledSet || emptySection.Index.Enabled {
		t.Fatalf("ParseFile: 空 [index] 段不应把 enabled 标为已设置，实际 set=%v enabled=%v",
			emptySection.Index.enabledSet, emptySection.Index.Enabled)
	}
}
