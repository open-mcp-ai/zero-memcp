package main

import (
	"os"
	"path/filepath"
	"testing"
)

// splitList 解析逗号分隔的多值参数（--mount、--remote 等都用它）。
//
// 判据是「用户怎么想就怎么算」：多余空格、空项、重复逗号都不该
// 变成空字符串塞进下游——那种空项会变成一个"没有名字的项目"。
func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{"a, b ,c", []string{"a", "b", "c"}},
		{" a ", []string{"a"}},
		{"a,,b", []string{"a", "b"}},
		{"a,", []string{"a"}},
		{",a", []string{"a"}},
	}
	for _, c := range cases {
		got := splitList(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitList(%q) = %v，期望 %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitList(%q) = %v，期望 %v", c.in, got, c.want)
			}
		}
	}
}

// expandHome 把开头的 ~ 换成家目录。
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	if got := expandHome("~"); got != home {
		t.Fatalf("expandHome(~) = %q，期望 %q", got, home)
	}
	if got, want := expandHome("~/x/y"), filepath.Join(home, "x/y"); got != want {
		t.Fatalf("expandHome(~/x/y) = %q，期望 %q", got, want)
	}
	// 绝对路径原样返回
	if got := expandHome("/tmp/x"); got != "/tmp/x" {
		t.Fatalf("绝对路径不该被改写，得到 %q", got)
	}
	// 空串原样返回
	if got := expandHome(""); got != "" {
		t.Fatalf("空串不该被改写，得到 %q", got)
	}
	// 相对路径原样返回
	if got := expandHome("rel/path"); got != "rel/path" {
		t.Fatalf("相对路径不该被改写，得到 %q", got)
	}
}

// resolveDataDir 决定 pidfile / 索引 / 仓库放在哪。
func TestResolveDataDir(t *testing.T) {
	// ① 显式 -data 优先，压过一切
	if got, err := resolveDataDir("/explicit/data", ""); err != nil || got != "/explicit/data" {
		t.Fatalf("显式 -data 应当优先，得到 %q, err=%v", got, err)
	}

	// ② 只给 -config 时，必须从配置文件里读，不能回退到 home
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zero-memcp.toml")
	cfgBody := "[server]\ndata = \"" + filepath.Join(dir, "fromcfg") + "\"\n" +
		"[[project]]\nname = \"me\"\n" +
		"[[user]]\nname = \"admin\"\npwd = \"me\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveDataDir("", cfgPath)
	if err != nil {
		t.Fatalf("合法配置应当成功: %v", err)
	}
	if want := filepath.Join(dir, "fromcfg"); got != want {
		t.Fatalf("应当从配置文件读 data=%q，实际得到 %q", want, got)
	}

	// ③ 都没有时回退到 ~/.zero-memcp/data
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		want := filepath.Join(home, ".zero-memcp", "data")
		if got, err := resolveDataDir("", ""); err != nil || got != want {
			t.Fatalf("默认应回退到 %q，实际 %q, err=%v", want, got, err)
		}
	}

	// ④ 失败必须出声：配置文件有语法错误时必须报错，绝不能静默换目录
	badCfg := filepath.Join(dir, "bad.toml")
	_ = os.WriteFile(badCfg, []byte("syntax error [[["), 0o600)
	if _, err := resolveDataDir("", badCfg); err == nil {
		t.Fatal("配置文件有错误时必须返回 error，绝不能静默回退")
	}
}
