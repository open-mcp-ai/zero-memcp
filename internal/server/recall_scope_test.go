package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// recall / grep / ls 的 --scope 必须真的生效，而且要支持分层写法 `A||B||C`。
//
// 为什么要有这条测试：`recall` 早先调 splitScope 拿到 scope 之后**没人用它**，
// 于是 `--scope` 后面的词被当成搜索词送进索引。症状是「召回结果莫名其妙」，
// 而不是报错——静默走偏比报错贵得多。
//
// 而 `A||B||C` 更糟：`||` 在 DSL 里是语句分隔符，整条命令被拆成
// 「recall ... --scope A」+「B」+「C」，后两段是非法指令。
// 扩展的分层召回一直发这个形状，所以**分层召回 100% 失败且被静默吞掉**：
// 调用方看到的是「没有命中」，于是以为记忆里真的没有。
func TestRecallScopeActuallyFilters(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 两个不同子树，各写一个独有词。
	if _, err := callMCP(t, srv, tok, "put blackegg/me@fact/alpha -- zzz-scope-alpha-marker"); err != nil {
		t.Fatalf("put alpha: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "put blackegg/me@fact/beta -- zzz-scope-beta-marker"); err != nil {
		t.Fatalf("put beta: %v", err)
	}

	// ① 单 scope 必须只回范围内的。
	out, err := callMCP(t, srv, tok, "recall zzz-scope --scope blackegg/me@fact/alpha")
	if err != nil {
		t.Fatalf("recall single scope: %v", err)
	}
	if !contains(out, "alpha") {
		t.Fatalf("范围内有内容却召回不到：%s", out)
	}
	if contains(out, "beta") {
		t.Fatalf("scope 没生效，范围外的 beta 也回来了：%s", out)
	}

	// ② 分层 scope `A||B` 必须两边都能回，且不能报错。
	out, err = callMCP(t, srv, tok, "recall zzz-scope --scope blackegg/me@fact/alpha||blackegg/me@fact/beta")
	if err != nil {
		t.Fatalf("recall layered scope 报错了（|| 被当成语句分隔符）：%v", err)
	}
	if !contains(out, "alpha") || !contains(out, "beta") {
		t.Fatalf("分层 scope 应当两边都命中：%s", out)
	}

	// ③ 范围外没命中时，要能区分「范围内没有」和「整个库没有」。
	out, err = callMCP(t, srv, tok, "recall zzz-scope-beta --scope blackegg/me@fact/alpha")
	if err != nil {
		t.Fatalf("recall outside scope: %v", err)
	}
	if contains(out, "no matches") && !contains(out, "elsewhere") {
		t.Fatalf("范围外有内容时应给出提示，而不是干巴巴的 no matches：%s", out)
	}
}

func TestGrepScopeActuallyFilters(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "put blackegg/me@fact/alpha -- zzz-grep-alpha"); err != nil {
		t.Fatalf("put alpha: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "put blackegg/me@fact/beta -- zzz-grep-beta"); err != nil {
		t.Fatalf("put beta: %v", err)
	}

	out, err := callMCP(t, srv, tok, "grep zzz-grep --scope blackegg/me@fact/alpha")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if contains(out, "beta") {
		t.Fatalf("grep scope 没生效：%s", out)
	}

	// 分层
	out, err = callMCP(t, srv, tok, "grep zzz-grep --scope blackegg/me@fact/alpha||blackegg/me@fact/beta")
	if err != nil {
		t.Fatalf("grep 分层 scope 报错：%v", err)
	}
	if !contains(out, "alpha") || !contains(out, "beta") {
		t.Fatalf("grep 分层 scope 应当两边都命中：%s", out)
	}
}

func TestSplitScopes(t *testing.T) {
	cases := []struct {
		in     string
		query  string
		scopes []string
	}{
		{"hello", "hello", nil},
		{"hello --scope a", "hello", []string{"a"}},
		{"hello --scope a||b||c", "hello", []string{"a", "b", "c"}},
		{"hello world --scope a||b", "hello world", []string{"a", "b"}},
		{"--scope a||b hello", "hello", []string{"a", "b"}},
		{"hello --scope a/ --scope b/", "hello", []string{"a", "b"}},
		{"hello --scope", "hello", nil},
	}
	for _, c := range cases {
		q, sc := splitScopes(c.in)
		if q != c.query {
			t.Errorf("%q: query = %q, want %q", c.in, q, c.query)
		}
		if len(sc) != len(c.scopes) {
			t.Errorf("%q: scopes = %v, want %v", c.in, sc, c.scopes)
			continue
		}
		for i := range sc {
			if sc[i] != c.scopes[i] {
				t.Errorf("%q: scopes = %v, want %v", c.in, sc, c.scopes)
			}
		}
	}
}