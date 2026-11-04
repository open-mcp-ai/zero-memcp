package server

import (
	"strings"
	"testing"
)

// TestPutSpecColonSyntax 锁死 put 的冒号地址解析：
//
// 冒号是 ref 与 path 的分界线（与 Git ref:path 同构）。
// 冒号前面的分支名可以含点（main.record），甚至含斜杠（task-x/thought）；
// 冒号后面的内容是树内路径，无论含不含斜杠都属于 path。
//
// 踩过的坑：早先 parsePutSpec 用 strings.Cut(rest, "/") 切分支，
// 导致 put weft@main.record:record/... 时，整串 'main.record:record'
// 被当作分支名，或者冒号没被处理被当成项目名误杀成
// 'outside this credential's visible scope'。
// 必须与 parseBranchAddr / memory.SplitID 走同一套规则。
func TestPutSpecColonSyntax(t *testing.T) {
	s := &Server{defaultBranch: "zero"}
	c := rootCaller()

	cases := []struct {
		target   string
		wantExp  string
		wantPath string
	}{
		{"weft@main.record:record", "weft@main.record", "record"},
		{"weft@main.record:record/2026-11-04", "weft@main.record", "record/2026-11-04"},
		{"weft@main.record:thought/2026-11-04/", "weft@main.record", "thought/2026-11-04"},
		{"weft@main:me/skills/01-weft", "weft@main", "me/skills/01-weft"},
		{"zero@task-x/thought:nested/path", "zero@task-x/thought", "nested/path"},
		// 旧语法兼容（无冒号时按第一个斜杠切）
		{"weft@main/record", "weft@main", "record"},
		{"weft@main", "weft@main", ""},
	}

	for _, tc := range cases {
		spec := s.parsePutSpec(tc.target, c)
		if spec.Explicit != tc.wantExp {
			t.Errorf("parsePutSpec(%q).Explicit = %q, want %q", tc.target, spec.Explicit, tc.wantExp)
		}
		if strings.Trim(spec.TreePath, "/") != strings.Trim(tc.wantPath, "/") {
			t.Errorf("parsePutSpec(%q).TreePath = %q, want %q", tc.target, spec.TreePath, tc.wantPath)
		}
	}
}
