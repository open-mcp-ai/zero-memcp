package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// issue #11：对外写动作的 provenance 账本。
//
// 验收标准（issue 原文）：
//  1. 给定 external_id，一条命令返回完整记录；
//  2. 给定不存在的 external_id，返回**明确的「无记录」**——不许静默空、
//     不许当错误抛出、不许返回 null 让调用方猜；
//  3. 同一 external_id 重复登记行为明确（拒绝并返回已有 / 更新并保留历史），
//     不能悄悄追加两条互相矛盾的记录。
func TestIssue11LedgerProvenance(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "desktop", Cap: "rwx"})

	// ① 不存在的 external_id：明确「无记录」，且不是错误。
	out, e := rawTerm(t, srv, tok, "ledger check forum-post-1", nil)
	if e != "" {
		t.Fatalf("查重不该把「无记录」当错误抛出: %s", e)
	}
	if !strings.Contains(out, "no record") {
		t.Fatalf("必须明确说出「无记录」，实际: %q", out)
	}

	// ② 写回执后能查出完整字段。
	if _, e := rawTerm(t, srv, tok,
		"ledger put forum-post-1 --result ok --channel forum --action post --identity identity-a", nil); e != "" {
		t.Fatalf("写回执失败: %s", e)
	}
	out, e = rawTerm(t, srv, tok, "ledger check forum-post-1", nil)
	if e != "" {
		t.Fatalf("查重失败: %s", e)
	}
	for _, want := range []string{"identity-a", "forum", "post", "result=ok", "at="} {
		if !strings.Contains(out, want) {
			t.Fatalf("回执必须含 %q，实际:\n%s", want, out)
		}
	}
	// actor 是发起动作的会话标识（取自凭证），必须能自证「谁发的」。
	if !strings.Contains(out, "actor=desktop") {
		t.Fatalf("回执必须记录 actor，实际:\n%s", out)
	}

	// ③ 重复登记同一 (external_id, result)：幂等，不追加第二条矛盾记录。
	if out2, e := rawTerm(t, srv, tok,
		"ledger put forum-post-1 --result ok --channel forum --action post --identity identity-a", nil); e != "" {
		t.Fatalf("重复登记不该失败: %s", e)
	} else if !strings.Contains(out2, "already recorded") {
		t.Fatalf("重复登记应返回已有记录并说明，实际: %q", out2)
	}
	checkOut, _ := rawTerm(t, srv, tok, "ledger check forum-post-1", nil)

	if n := strings.Count(checkOut, "\n"); n != 0 {
		t.Fatalf("同一结果重复登记后不该有两条记录，实际:\n%s", checkOut)
	}

	// ④ 不同 result（失败 → 成功）保留历史：重试成功本身是要审计的事实。
	if _, e := rawTerm(t, srv, tok, "ledger put forum-post-2 --result failed:timeout", nil); e != "" {
		t.Fatal(e)
	}
	if _, e := rawTerm(t, srv, tok, "ledger put forum-post-2 --result ok", nil); e != "" {
		t.Fatal(e)
	}
	two, _ := rawTerm(t, srv, tok, "ledger check forum-post-2", nil)
	if !strings.Contains(two, "result=failed:timeout") || !strings.Contains(two, "result=ok") {
		t.Fatalf("失败与成功两条记录都必须保留:\n%s", two)
	}

	// ⑤ 失败也要写：不写 result 的回执必须被拒（否则重试查重会误判）。
	if _, e := rawTerm(t, srv, tok, "ledger put forum-post-3 --channel forum", nil); e == "" {
		t.Fatal("缺 --result 的回执必须被拒")
	}
	// 客户端不能自报发生时间（at 只能由服务端产生）。
	if _, e := rawTerm(t, srv, tok, "ledger put forum-post-3 --result ok --at 2000-01-01T00:00:00Z", nil); e == "" {
		t.Fatal("客户端自报 --at 必须被拒")
	}

	// ⑥ list 能按 identity 与时间审计。
	listOut, e := rawTerm(t, srv, tok, "ledger list --identity identity-a", nil)
	if e != "" {
		t.Fatalf("ledger list 失败: %s", e)
	}
	if !strings.Contains(listOut, "identity-a") || strings.Contains(listOut, "identity=desktop") {
		t.Fatalf("list 必须按 identity 过滤:\n%s", listOut)
	}

	// ⑦ 账本条目就是普通记忆条目：能被 ls 列出来，也能用 read 原样读出。
	lsOut, e := rawTerm(t, srv, tok, "ls", nil)
	if e != "" || !strings.Contains(lsOut, "ledger/") {
		t.Fatalf("账本条目应作为普通条目出现在 ls 里: err=%q out=%q", e, lsOut)
	}
	// 取第一行里的地址读全文，并确认里面绝不存凭证字样。
	addr := strings.Fields(checkOut)[0]
	rawBody, e := rawTerm(t, srv, tok, "read "+addr, nil)
	if e != "" || !strings.Contains(rawBody, "identity: identity-a") {
		t.Fatalf("账本条目应能用 read 直接读回: err=%q out=%q", e, rawBody)
	}
	if strings.Contains(strings.ToLower(rawBody), "token:") || strings.Contains(strings.ToLower(rawBody), "secret:") {
		t.Fatalf("账本不得存凭证:\n%s", rawBody)
	}
}
