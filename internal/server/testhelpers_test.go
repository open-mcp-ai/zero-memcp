package server

import "strings"

// contains 是测试里的子串判定。
//
// 放在这个不带构建标签的文件里，是因为常规测试与性能测试都要用它。
// 早先它定义在 incremental_write_perf_test.go 里，给那个文件加上
// `//go:build perf` 之后，常规测试立刻编译失败（undefined: contains）——
// 共享的测试辅助不能住在被标签隔离的文件里。
func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
