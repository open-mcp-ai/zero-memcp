package config

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

func mustParse(t *testing.T, src string) *File {
	t.Helper()
	f, err := ParseFile([]byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v\n---\n%s", err, src)
	}
	return f
}

func TestParseMinimal(t *testing.T) {
	f := mustParse(t, `
[[user]]
name = "alice"
root = true
`)
	if len(f.Users) != 1 || f.Users[0].Name != "alice" || !f.Users[0].Root {
		t.Fatalf("用户解析错: %+v", f.Users)
	}
	// 缺省值必须生效，否则没人配置的环境下服务行为会不确定。
	if f.Server.Listen != "127.0.0.1:1729" {
		t.Fatalf("缺省监听地址错: %q", f.Server.Listen)
	}
	if f.Commit.Interval != 30*time.Second || f.Commit.Count != 50 {
		t.Fatalf("缺省提交策略错: %+v", f.Commit)
	}
}

func TestParseFull(t *testing.T) {
	f := mustParse(t, `
# 注释行
[server]
data     = "/data"          # 行尾注释
listen   = "127.0.0.1:1729"
endpoint = "blackbox"

[commit]
interval = "5m"
count    = 200

[gc]
interval = "6h"

[[user]]
name   = "alice"
root   = true

[[user]]
name   = "qq-bot"
groups = ["bots", "family"]

[[group]]
name    = "bots"
members = ["qq-bot"]

[[group]]
name = "family"

[[project]]
name  = "me"
owner = "alice"
mode  = "0755"

[[project]]
name  = "daily"
owner = "alice"
group = "family"
mode  = "rwxr-x---"

[[access]]
user    = "qq-bot"
project = "daily"
perm    = "rw"
`)

	if f.Server.Data != "/data" || f.Server.Endpoint != "blackbox" {
		t.Fatalf("server 解析错: %+v", f.Server)
	}
	if f.Commit.Interval != 5*time.Minute || f.Commit.Count != 200 {
		t.Fatalf("commit 解析错: %+v", f.Commit)
	}
	if f.GC.Interval != 6*time.Hour {
		t.Fatalf("gc 解析错: %+v", f.GC)
	}
	if len(f.Users) != 2 || len(f.Groups) != 2 || len(f.Projects) != 2 || len(f.Access) != 1 {
		t.Fatalf("条目数错: users=%d groups=%d projects=%d access=%d",
			len(f.Users), len(f.Groups), len(f.Projects), len(f.Access))
	}
	// 两种权限写法必须等价
	if f.Projects[1].Perm.String() != "rwxr-x---" {
		t.Fatalf("九字符权限解析错: %s", f.Projects[1].Perm.String())
	}
	if f.Projects[0].Perm.Octal() != "0755" {
		t.Fatalf("八进制权限解析错: %s", f.Projects[0].Perm.Octal())
	}
}

// 引号里的 # 不是注释。这条错了会让带锚点的 URL 被截断。
func TestHashInsideStringIsNotComment(t *testing.T) {
	f := mustParse(t, `
[server]
data = "/data#with#hash"
[[user]]
name = "a"
`)
	if f.Server.Data != "/data#with#hash" {
		t.Fatalf("字符串里的 # 被当成注释了: %q", f.Server.Data)
	}
}

// 未知分节不该报错（留给将来扩展），但未知键在已声明的分节里应报错。
func TestUnknownKeyInSectionRejected(t *testing.T) {
	_, err := ParseFile([]byte(`
[server]
data = "/d"
listenn = "1.2.3.4:9"
[[user]]
name = "a"
`))
	if err == nil {
		t.Fatal("未知键应报错，否则拼写错误会被静默忽略")
	}
	if !strings.Contains(err.Error(), "listenn") {
		t.Fatalf("错误信息应指出键名: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"没有用户": `
[[project]]
name = "me"
`,
		"用户重复": `
[[user]]
name = "a"
[[user]]
name = "a"
`,
		"引用不存在的组": `
[[user]]
name = "a"
groups = ["nope"]
`,
		"组员不是用户": `
[[user]]
name = "a"
[[group]]
name = "g"
members = ["ghost"]
`,
		"owner 不是用户": `
[[user]]
name = "a"
[[project]]
name = "me"
owner = "ghost"
`,
		"project group 不存在": `
[[user]]
name = "a"
[[project]]
name = "me"
group = "nope"
`,
		"access 指向不存在的用户": `
[[user]]
name = "a"
[[project]]
name = "me"
[[access]]
user = "ghost"
project = "me"
perm = "r"
`,
		"access perm 非法": `
[[user]]
name = "a"
[[project]]
name = "me"
[[access]]
user = "a"
project = "me"
perm = "rwz"
`,
	}
	for name, src := range cases {
		if _, err := ParseFile([]byte(src)); err == nil {
			t.Fatalf("%s：应报错", name)
		}
	}
}

func TestDurationRejects(t *testing.T) {
	_, err := ParseFile([]byte(`
[commit]
interval = "30 seconds"
[[user]]
name = "a"
`))
	if err == nil {
		t.Fatal("非法时长应报错")
	}
}

// ---- 权限判定 ----

func permModel(t *testing.T) *Lookup {
	return mustParse(t, `
[[user]]
name = "owner"
root = true

[[user]]
name = "alice"
groups = ["family"]

[[user]]
name = "bob"
groups = ["family"]

[[user]]
name = "stranger"

[[group]]
name = "family"

[[project]]
name  = "me"
owner = "alice"
group = "family"
mode  = "rwxr-x---"

[[project]]
name  = "public"
owner = "alice"
mode  = "rwxr-xr-x"

[[project]]
name  = "private"
owner = "alice"
mode  = "rwx------"

[[access]]
user    = "stranger"
project = "private"
perm    = "r"
`).Build()
}

func TestCheckRootBypassesEverything(t *testing.T) {
	l := permModel(t)
	for _, p := range []string{"me", "public", "private"} {
		for _, perm := range []uint8{auth.BitRead, auth.BitWrite, auth.BitExecute} {
			if !Check(l, "owner", p, perm) {
				t.Fatalf("root 应对 %s 有全部权限", p)
			}
		}
	}
}

func TestCheckOwnerGroupOther(t *testing.T) {
	l := permModel(t)

	// 属主：rwx
	if !Check(l, "alice", "me", auth.BitWrite) {
		t.Fatal("属主应可写")
	}
	// 同组：r-x，不能写
	if !Check(l, "bob", "me", auth.BitRead) {
		t.Fatal("同组应可读")
	}
	if Check(l, "bob", "me", auth.BitWrite) {
		t.Fatal("同组不应可写")
	}
	// 其他位是 ---，陌生人读写都不行
	if Check(l, "stranger", "me", auth.BitRead) {
		t.Fatal("其他位 --- 不应可读")
	}
	// 其他位是 r-x 的项目，陌生人可读不可写
	if !Check(l, "stranger", "public", auth.BitRead) {
		t.Fatal("public 其他人应可读")
	}
	if Check(l, "stranger", "public", auth.BitWrite) {
		t.Fatal("public 其他人不应可写")
	}
}

// 显式 access 覆盖 mode —— 这是「给某人开小灶」的唯一途径。
func TestAccessOverridesMode(t *testing.T) {
	l := permModel(t)

	// private 的 other 位是 ---，但 stranger 被显式授了 r
	if !Check(l, "stranger", "private", auth.BitRead) {
		t.Fatal("显式授权应覆盖 mode")
	}
	// 但显式授权只写了 r，写仍然不行
	if Check(l, "stranger", "private", auth.BitWrite) {
		t.Fatal("显式授权只有 r，不该可写")
	}
}

// 未在配置里声明的项目，任何人都不该能碰。
func TestUnknownProjectDenied(t *testing.T) {
	l := permModel(t)
	if Check(l, "owner", "ghost", auth.BitRead) {
		t.Fatal("未声明的项目连 root 也不该自动获得——root 是绕过位检查，不是绕过存在性")
	}
}

// 组的两个来源（user.groups 与 group.members）必须取并集。
func TestGroupsOfUnion(t *testing.T) {
	l := mustParse(t, `
[[user]]
name = "a"
groups = ["g1"]
[[user]]
name = "b"
[[group]]
name = "g1"
[[group]]
name = "g2"
members = ["b"]
`).Build()

	if got := l.GroupsOf("a"); len(got) != 1 || got[0] != "g1" {
		t.Fatalf("a 的组错: %v", got)
	}
	if got := l.GroupsOf("b"); len(got) != 1 || got[0] != "g2" {
		t.Fatalf("b 的组错: %v", got)
	}
}

// 属主被否定时不能退回组位——POSIX 的规则，排除了「算哪边」的歧义。
func TestOwnerDeniedDoesNotFallThroughToGroup(t *testing.T) {
	l := mustParse(t, `
[[user]]
name = "alice"
groups = ["family"]
[[group]]
name = "family"
[[project]]
name  = "p"
owner = "alice"
group = "family"
mode  = "rwx---r--"
`).Build()

	// alice 是属主，属主位是 rwx。即使组位是 --- 也不影响她。
	if !Check(l, "alice", "p", auth.BitWrite) {
		t.Fatal("属主应可写")
	}
}
