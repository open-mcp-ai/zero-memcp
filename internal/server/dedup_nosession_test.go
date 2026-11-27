package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// TestDedupIgnoresSession 钉住「纯对话判重」：不同会话里的同一段内容，
// 只落一次盘。
//
// 事故复盘（2026-11-23）：
// Pi 的压缩机制会生成新会话文件，新文件里带着完整逐字历史（实测
// 子文件首条消息与父文件同时间戳、同内容，两份文件内容重合 99.3%）。
// 早先 dedupKey 里含 session，重放历史时每个新会话都被当成全新记录
// 全量落盘：28 个会话一条链累计 80,290 轮回答，去重后只有 6,498 轮，
// 重复占 91.9%；存储里同一句「读一下这个项目」出现过 24 份。
//
// 修复后指纹 = project + branch + kind + 正文（不含 session），
// 且落盘内容里不再写 `session:` —— 落盘就是纯对话。
func TestDedupIgnoresSession(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	// @sess-A / @sess-B 是 put 的头部会话标记
	const body = "标记 ZMNOSESS 全局无锁：状态靠 channel 与 atomic"
	if _, err := callMCPWithVars(t, srv, tok, "put blackegg/daily --session sess-A $TEXT",
		map[string]string{"TEXT": body}); err != nil {
		t.Fatalf("第一次写入失败: %v", err)
	}
	// 换一个完全不同的会话 id，正文一字不差
	if _, err := callMCPWithVars(t, srv, tok, "put blackegg/daily --session sess-B $TEXT",
		map[string]string{"TEXT": body}); err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}

	out, err := callMCP(t, srv, tok, "grep -c ZMNOSESS")
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if !strings.Contains(out, "1 block(s)") {
		t.Fatalf("不同 session 写入相同正文应只落一份（会话压缩后重放历史不该产生重复），实际:\n%s", out)
	}
}

// TestDedupDifferentiatesDifferentText 确认判重不会把不同正文误判成同一条。
func TestDedupDifferentiatesDifferentText(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCPWithVars(t, srv, tok, "put blackegg/daily --session sess-A $TEXT",
		map[string]string{"TEXT": "标记 ZMDIFF 甲"}); err != nil {
		t.Fatalf("写入甲失败: %v", err)
	}
	if _, err := callMCPWithVars(t, srv, tok, "put blackegg/daily --session sess-A $TEXT",
		map[string]string{"TEXT": "标记 ZMDIFF 乙"}); err != nil {
		t.Fatalf("写入乙失败: %v", err)
	}

	out, err := callMCP(t, srv, tok, "grep -c ZMDIFF")
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if !strings.Contains(out, "2 block(s)") {
		t.Fatalf("两段不同正文应各落一份，实际:\n%s", out)
	}
}

// TestStoredSliceCarriesSessionForProvenance 钉住新契约：
// session 写进 frontmatter 当溯源，但**不参与判重**。
//
// 2026-11-23 为了压掉压缩分叉产生的 91.9% 重复，把 session 同时
// 赶出了判重键和 frontmatter。前一半必须留（可重键只能看内容），
// 后一半是附带损失：同一分支上再也没法回答「这几条来自哪个会话」。
// 现在拆开：落盘带 session，判重仍只看内容。
func TestStoredSliceCarriesSessionForProvenance(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	out, err := callMCPWithVars(t, srv, tok, "put blackegg/daily --session sess-X $TEXT",
		map[string]string{"TEXT": "标记 ZMFRONT 纯对话"})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	addr := firstAddrIn(out)
	if addr == "" {
		t.Fatalf("没从 put 返回里解析出地址: %s", out)
	}
	body, err := callMCP(t, srv, tok, "read "+addr)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(body, "session: sess-X") {
		t.Fatalf("落盘内容应带 session 溯源行，实际:\n%s", body)
	}
	if !strings.Contains(body, "ZMFRONT") {
		t.Fatalf("读取到的内容不含正文，说明地址解析或写入有问题:\n%s", body)
	}
}

// firstAddrIn 从 put 的返回文案里抽出第一个地址。
// 形如 "ok blackegg/daily/2026-11-23/xxxx.md (base c12cdb91)"。
func firstAddrIn(msg string) string {
	const prefix = "ok "
	i := strings.Index(msg, prefix)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(prefix):]
	if sp := strings.IndexAny(rest, " \n"); sp > 0 {
		return rest[:sp]
	}
	return strings.TrimSpace(rest)
}

// TestDedupHealsAfterDelete 钉住「删掉之后重写同一段正文必须成功」。
//
// 故障现场（2026-11-23，真实清理操作触发）：
//
//	write reported success but record/…/151338.525161-7451-laptop.md
//	cannot be read back (… does not exist); treating it as failed
//
// 成因：批量 delete 之后，内存里的判重表仍缓存着被删文件的路径。
// 此时写入一模一样的正文，判重命中直接返回那个已删路径，随后的
// 回读校验发现文件不存在，于是一次正常写入被判失败——写丢失。
//
// 这不是理论情况：清理 5,315 条重复切片后重导缺失轮次，62 条里
// 有 22 条就是这么丢的。
//
// 修复后：命中判重时先确认文件真的还在；不在就淘汰陈旧条目，穿透写入。
func TestDedupHealsAfterDelete(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	const body = "标记 ZMGHOST 这段正文会被删掉再原样写回来"

	out, err := callMCPWithVars(t, srv, tok, "put blackegg/daily $TEXT",
		map[string]string{"TEXT": body})
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	addr := firstAddrIn(out)
	if addr == "" {
		t.Fatalf("没解析出地址: %s", out)
	}
	// addr 形如 blackegg/daily/2026-11-23/xxx.md；apply 要的是树内路径
	rel := strings.TrimPrefix(addr, "blackegg/daily/")

	patch := `[{"op":"delete","path":` + string(mustJSON(t, rel)) + `}]`
	if _, err := callMCPWithVars(t, srv, tok, "apply blackegg/daily@zero $PATCHES",
		map[string]string{"PATCHES": patch}); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// 原样再写一次：必须真的写成功，而不是命中一个已不存在的旧路径
	if _, err := callMCPWithVars(t, srv, tok, "put blackegg/daily $TEXT",
		map[string]string{"TEXT": body}); err != nil {
		t.Fatalf("删除后重写同一段正文必须成功。判重命中了已删除的陈旧路径会导致写丢失，实际错误: %v", err)
	}

	gout, err := callMCP(t, srv, tok, "grep -c ZMGHOST")
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if !strings.Contains(gout, "1 block(s)") {
		t.Fatalf("删除后应恰好存在一份，实际:\n%s", gout)
	}
}

// mustJSON 把一个字符串编码成 JSON 字面量（测试内小工具）。
func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json 编码失败: %v", err)
	}
	return b
}
