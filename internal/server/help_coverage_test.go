package server

import (
	"strings"
	"testing"
)

// 每个在 DSL 里分发的动词都必须有 help 词条。
//
// 这条测试盯的是一类会反复出现的漂移：加了新动词，忘了加手册。
// 症状很轻——`help commit` 回一句「没有 commit 这个动词」，
// 而动词本身工作正常，所以谁都不会当成 bug 报上来。
//
// 维护方式：新增动词时把名字加进 dispatchVerbs。
func TestEveryDispatchedVerbHasHelp(t *testing.T) {
	// 与 server.go 的 switch verb 保持一致（不含内部/调试动词）。
	dispatchVerbs := []string{
		"help", "recall", "read", "put", "remind", "trees", "since",
		"remote-key", "remote-secret", "remote", "fetch", "push", "sync",
		"branch", "tag", "cp", "mv", "chmod", "delete",
		"lock", "unlock", "archive", "reset", "batch", "diff",
		"commit", "write", "config", "reload", "whoami", "users", "status", "grep", "log", "ls",
	}

	for _, v := range dispatchVerbs {
		detail, ok := helpVerb(v)
		if !ok {
			t.Errorf("动词 %q 有实现但没有 help 词条（help %s 会报「没有这个动词」）", v, v)
			continue
		}
		if strings.TrimSpace(detail) == "" {
			t.Errorf("动词 %q 的 help 词条是空的", v)
		}
	}
}

// help --all 必须把每个有词条的动词都带上，而不是漏掉几个。
func TestHelpAllCoversEveryVerb(t *testing.T) {
	all := helpAll()
	for _, v := range []string{"commit", "diff", "branch", "tag", "cp", "mv", "delete", "archive", "reset", "batch", "write", "grep"} {
		if !strings.Contains(all, "── "+v+" ") {
			t.Errorf("help --all 缺少 %q 的章节", v)
		}
	}
}
