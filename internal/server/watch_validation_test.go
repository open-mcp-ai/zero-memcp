package server

import (
	"os"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// remind 必须拒绝「地址 + 多余参数」。
//
// 要防的具体故障（3060 hub 实盘见过）：watches.json 里出现
//
//	branch = "main.shared e8103f1809f417e58dc667d977fb92351e73b036"
//
// 成因是把 `since <tree> <commit>` 的写法用在了 `remind` 上，多出来的
// hash 被整段吞进分支名。它不报错，只留下一条**永远匹配不上的订阅**：
// status 永远说「没变化」，而分支其实一直在动。
func TestRemindRejectsExtraArguments(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	out, err := callMCP(t, srv, tok,
		"remind blackegg/me@fact e8103f1809f417e58dc667d977fb92351e73b036")
	if err == nil {
		t.Fatalf("remind 带多余参数应当报错，实际成功：%q", out)
	}
	if !strings.Contains(err.Error(), "since") {
		t.Fatalf("报错必须指向正确的动词 `since`，实际：%v", err)
	}
	t.Logf("拒绝并给出提示：%v", err)
}

// 正常的单地址 remind 必须仍然工作（别为了拦脏输入把正路堵死）。
func TestRemindAcceptsSingleAddress(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "w", Cap: "rwx"})

	out, err := callMCP(t, srv, tok, "remind blackegg/me@fact")
	if err != nil {
		t.Fatalf("正常 remind 不应失败：%v", err)
	}
	if !strings.Contains(out, "watching") {
		t.Fatalf("应当确认已开始关注，实际：%q", out)
	}
	// 带路径的关注也要能过
	if _, err := callMCP(t, srv, tok, "remind blackegg/me@fact:some/sub"); err != nil {
		t.Fatalf("带路径的 remind 不应失败：%v", err)
	}
}

// 兜底校验：任何写入路径都不可能落进带空白/非法字符的分支名。
func TestTreeWatchRejectsMalformedBranch(t *testing.T) {
	hub := newTreeWatchHub()

	bad := [][3]string{
		{"proj", "main.shared e8103f18", ""}, // 带空格的 ref 在 Git 里不存在
		{"proj", "main\u0000x", ""},
		{"proj", "main..dev", ""}, // Git 明确禁止
		{"proj", "main~1", ""},
		{"proj", "/main", ""},
		{"proj", "main/", ""},
		{"proj", "", ""}, // 必须指明分支
		{"", "main", ""}, // 必须指明项目
	}
	for _, b := range bad {
		if err := hub.Add("zero", TreeWatch{Project: b[0], Branch: b[1], Path: b[2]}); err == nil {
			t.Fatalf("非法关注目标 %v 应当被拒绝", b)
		}
	}

	good := [][3]string{
		{"blackegg/me", "fact", ""},
		{"BlackEggAI/zero", "main.shared", ""},
		{"BlackEggAI/zero", "main", "some/sub"},
	}
	for _, g := range good {
		if err := hub.Add("zero", TreeWatch{Project: g[0], Branch: g[1], Path: g[2]}); err != nil {
			t.Fatalf("合法关注目标 %v 不应被拒绝：%v", g, err)
		}
	}
}

// 加载时必须清洗历史遗留的幽灵订阅，并且把清洗结果写回磁盘。
//
// 3060 hub 上真实存在过这样一条：
//
//	branch = "main.shared e8103f1809f417e58dc667d977fb92351e73b036"
//
// 丢弃要出声、要落盘：静默清理会让「我明明 remind 过」变成无从查起的谜，
// 而不落盘则每次重启都报一遍。
func TestWatchLoadDropsGhostEntries(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/watches.json"
	body := `{"watches":{"zero":[
		{"project":"BlackEggAI/zero","branch":"main.shared","path":"","since":"abc123"},
		{"project":"BlackEggAI/zero","branch":"main.shared e8103f1809f417e58dc667d977fb92351e73b036","path":""}
	]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	hub, err := newPersistentTreeWatchHub(path)
	if err != nil {
		t.Fatalf("加载不应失败（清洗是修复而不是报错）：%v", err)
	}
	list := hub.List("zero")
	if len(list) != 1 {
		t.Fatalf("应当只剩 1 条合法订阅，实际 %d 条：%+v", len(list), list)
	}
	if list[0].Branch != "main.shared" {
		t.Fatalf("留下的应当是合法那条，实际 %q", list[0].Branch)
	}

	// 必须把清洗结果写回磁盘，否则每次重启都重复报一遍
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "e8103f18") {
		t.Fatalf("清洗后应当已重写文件，幽灵条目仍在：%s", saved)
	}
	if !strings.Contains(string(saved), "main.shared") {
		t.Fatalf("合法条目不应被写丢：%s", saved)
	}
}
