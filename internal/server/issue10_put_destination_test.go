package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// issue #10：无前缀 put 静默落默认树、返回体不给完整落点、没有 dry-run。
//
// 真实事故：一份规范地址在 @ops 的活表被无前缀 put 写进了 @main，
// 多出一份重复副本；两张表并存期间 grep 同时命中两份，事后靠人工比对
// 才收拾干净。写错一秒，收拾要人工比对两份文档。
func TestIssue10PutReportsDestinationAndSupportsWhere(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// ① 返回体必须带完整落点 <project>@<branch>:<path>，而不只是文件名。
	out, e := rawTerm(t, srv, tok, "put $B", map[string]string{"B": "落点可见性"})
	if e != "" {
		t.Fatalf("put 失败: %s", e)
	}
	if !strings.Contains(out, "blackegg/me@zero:") {
		t.Fatalf("put 返回体必须给出完整落点，实际: %q", out)
	}
	// 没指定树时必须标明「这是服务端替我选的」，不能看起来像我自己选的。
	if !strings.Contains(out, "default tree") {
		t.Fatalf("无前缀 put 必须标明默认落点，实际: %q", out)
	}

	// ② 指定了树时不得再谎称 default。
	out2, e := rawTerm(t, srv, tok, "put blackegg/me/todo $B", map[string]string{"B": "指定落点"})
	if e != "" {
		t.Fatalf("put 指定树失败: %s", e)
	}
	if strings.Contains(out2, "default tree") {
		t.Fatalf("显式落点不该被标成默认: %q", out2)
	}
	if !strings.Contains(out2, "blackegg/me@zero:todo/") {
		t.Fatalf("显式落点应含树前缀: %q", out2)
	}

	// ③ --where 写前可探：给出同一份答案，且**不写任何东西**。
	probe, e := rawTerm(t, srv, tok, "put --where", nil)
	if e != "" {
		t.Fatalf("put --where 失败: %s", e)
	}
	if !strings.Contains(probe, "blackegg/me@zero") {
		t.Fatalf("探针必须给出落点: %q", probe)
	}
	if !strings.Contains(probe, "default for this credential") {
		t.Fatalf("探针必须说明落点由谁决定: %q", probe)
	}
	// 不落盘：探针前后计数不变。
	before, _ := rawTerm(t, srv, tok, "grep -c 探针不该写进去", nil)
	if _, e := rawTerm(t, srv, tok, "put --where", nil); e != "" {
		t.Fatal(e)
	}
	after, _ := rawTerm(t, srv, tok, "grep 探针不该写进去", nil)
	if before != after {
		t.Fatalf("--where 不得写入任何东西: before=%q after=%q", before, after)
	}
	// 探针形态的几种写法都要能过（含 --dry-run 与带目标树）。
	for _, cmd := range []string{"put --dry-run", "put --where blackegg/me", "put blackegg/me --where", "put --where blackegg/me/todo"} {
		if _, e := rawTerm(t, srv, tok, cmd, nil); e != "" {
			t.Fatalf("%s 必须能执行: %s", cmd, e)
		}
	}
	// 探针结果必须与真实写入的落点一致（共用 planWrite，不能两套答案）。
	probeTree, _ := rawTerm(t, srv, tok, "put --where blackegg/me/todo", nil)
	if !strings.Contains(probeTree, "blackegg/me@zero/todo") {
		t.Fatalf("探针的落点必须与写入一致（含树前缀）: %q", probeTree)
	}
}
