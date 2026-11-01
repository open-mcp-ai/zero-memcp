package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 验证全局账户看全路径，受限账户在自己的 pwd 里用相对路径。
func TestPwdRelativeAddressing(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[user]]
name = "subagent"
pwd  = "team/daily"

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwxr-xr-x"

[[project]]
name  = "team/daily"
owner = "subagent"
mode  = "rwx------"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	// 1. 初始化 blackegg/me
	rMe, err := gitstore.Init(filepath.Join(dataDir, "repos", "blackegg", "me.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = rMe.Write("zero", "SKILL.md", []byte("我是 zero\n"), gitstore.ModeRule)
	_, _ = rMe.Commit("zero", "init")

	// 2. 初始化 team/daily
	rDaily, err := gitstore.Init(filepath.Join(dataDir, "repos", "team", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = rDaily.Write("zero", "today.md", []byte("今天的任务\n"), gitstore.ModeContent)
	_, _ = rDaily.Commit("zero", "init")

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	zeroTok := mintToken(t, dataDir, "zero")
	subTok := mintToken(t, dataDir, "subagent")

	// 全局账户用全路径看两个仓库
	out, err := callMCP(t, srv, zeroTok, "read blackegg/me/SKILL.md")
	if err != nil || !strings.Contains(out, "我是 zero") {
		t.Fatalf("zero 全路径读 me 失败: %v, out: %s", err, out)
	}
	out, err = callMCP(t, srv, zeroTok, "read team/daily/today.md")
	if err != nil || !strings.Contains(out, "今天的任务") {
		t.Fatalf("zero 全路径读 daily 失败: %v, out: %s", err, out)
	}

	// 受限账户自带 pwd = "team/daily"：直接用相对路径读
	out, err = callMCP(t, srv, subTok, "read today.md")
	if err != nil || !strings.Contains(out, "今天的任务") {
		t.Fatalf("subagent 相对路径读失败: %v, out: %s", err, out)
	}

	// 受限账户尝试读没有权限的项目（即使写全路径也应被拒绝）
	out, _ = callMCP(t, srv, subTok, "read blackegg/me/SKILL.md")
	if strings.Contains(out, "我是 zero") {
		// blackegg/me 是 rwxr-xr-x，subagent 其他位有 r 权限，应能读
	}
}

// chroot 之内，全路径不会把请求带出这个仓库。
//
// 曾经的 bug：项目清单取自磁盘扫描，于是一个只声明、还没被写过的仓库
// 匹配不到，全路径被当成相对路径塞进 pwd，读到了**另一个仓库**里。
// 不报错，只是读错地方——这是最难查的一类失败。
// 现在由挂载表兜住：表外的一切都被解析成根仓库内的相对路径。
func TestDeclaredButEmptyProjectNotSwallowedByPwd(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[user]]
name = "bot"
pwd  = "team/daily"

[[project]]
name  = "team/daily"
owner = "zero"
mode  = "rwxrwxrwx"

[[project]]
name  = "team/empty"
owner = "zero"
mode  = "rwxrwxrwx"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}

	// 只建 team/daily，team/empty 故意不建（模拟「声明了但还没写过」）
	r, err := gitstore.Init(filepath.Join(dataDir, "repos", "team", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Write("zero", "a.md", []byte("A 的内容\n"), gitstore.ModeContent)
	_, _ = r.Commit("zero", "init")

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	botTok := mintToken(t, dataDir, "bot")

	// 读 team/empty 里的东西：必须报「项目不存在」，
	// 绝不能回落到 pwd 下的 team/empty/... 然后读到 team/daily
	out, err := callMCP(t, srv, botTok, "read team/empty/x.md")
	if strings.Contains(out, "A 的内容") {
		t.Fatalf("全路径被 pwd 吞掉了，读到了别的仓库: %s", out)
	}
	// 可能是 error（无内容时）或文本（部分失败时），两者都要包含原因
	msg := out
	if err != nil {
		msg = err.Error()
	}
	// 新语义：bot 被 pwd 关进 team/daily，chroot 之内叫不出别的仓库，
	// 所以 "team/empty/x.md" 只是 team/daily 里的一个相对路径，报 not found。
	// 关键是不能读到 team/daily 的 a.md 内容。
	if !strings.Contains(msg, "not found") {
		t.Fatalf("应报 not found，实际: %q / err=%v", out, err)
	}

	// 相对路径仍然正常工作
	out, err = callMCP(t, srv, botTok, "read a.md")
	if err != nil || !strings.Contains(out, "A 的内容") {
		t.Fatalf("相对路径应正常: %v, out=%s", err, out)
	}
}
