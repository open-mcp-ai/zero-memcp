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
func TestIndexAsyncDefaultsOn(t *testing.T) {
	var c IndexConfig
	if !c.AsyncBuild() {
		t.Fatal("未配置 async 时应默认后台构建")
	}
	c.Async, c.asyncSet = false, true
	if c.AsyncBuild() {
		t.Fatal("显式 async = false 时必须尊重")
	}
}
