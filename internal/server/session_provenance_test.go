package server

import (
	"context"
	"strings"
	"testing"
)

// session 写进 frontmatter 当溯源，但不参与判重。
//
// 这两件事必须分开，而 2026-11-23 那次改动把它们绑在一起砍了：
// 为了不让压缩分叉把同一段对话重写成 N 条，连 session 字段本身
// 也不落盘了。结果是同一条分支上再也没法回答「这几条来自哪个会话」。
//
// 下面三条钉住边界：该留的留（溯源），该判的照判（内容判重）。
func TestSessionRecordedInFrontmatterButNotDeduped(t *testing.T) {
	s, _ := newTestServer(t, `
[server]
data = "."

[[project]]
name = "notes"
owner = "alice"
mode = "0755"

[[user]]
name = "alice"
root = true
`)

	res1, err := s.ExecLineVars(context.Background(), "put --session sess-alpha $1", nil, []string{"同一个答案"})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	addr1 := strings.TrimPrefix(strings.TrimSpace(res1), "ok ")
	addr1 = strings.Fields(addr1)[0]

	raw, err := s.ExecLine(context.Background(), "read "+addr1)
	if err != nil {
		t.Fatalf("读回切片失败: %v", err)
	}
	if !strings.Contains(raw, "session: sess-alpha") {
		t.Fatalf("frontmatter 里没有 session 溯源行:\n%s", raw)
	}

	// 同一段正文换一个会话再写一次：仍然是同一条记忆，返回已有地址。
	// 如果这里生成了新文件，说明 session 漏进了判重键，压缩分叉的
	// 91.9% 重复会立刻回来。
	res2, err := s.ExecLineVars(context.Background(), "put --session sess-beta $1", nil, []string{"同一个答案"})
	if err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}
	addr2 := strings.TrimPrefix(strings.TrimSpace(res2), "ok ")
	addr2 = strings.Fields(addr2)[0]
	if addr2 != addr1 {
		t.Fatalf("session 混进了判重键：同一段正文换会话后生成了新切片 %q（原 %q）", addr2, addr1)
	}
}

// 没有会话的写入（write 动词、库调用方）不该留下空的 session 行。
//
// 空行会让「按 session 过滤」变成一个大杂烩：所有无会话切片都命中
// 同一个空值，看起来像一个真实会话。
func TestSessionLineOmittedWhenEmpty(t *testing.T) {
	s, _ := newTestServer(t, `
[server]
data = "."

[[project]]
name = "notes"
owner = "alice"
mode = "0755"

[[user]]
name = "alice"
root = true
`)

	res, err := s.ExecLineVars(context.Background(), "put $1", nil, []string{"没有会话的切片"})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	addr := strings.TrimPrefix(strings.TrimSpace(res), "ok ")
	addr = strings.Fields(addr)[0]

	raw, err := s.ExecLine(context.Background(), "read "+addr)
	if err != nil {
		t.Fatalf("读回切片失败: %v", err)
	}
	if strings.Contains(raw, "session:") {
		t.Fatalf("无会话的写入不该留下 session 行:\n%s", raw)
	}
}

// session 必须能被检索到。
//
// 写路径索引的是整份文件（含 frontmatter），重建路径也是——两边必须
// 一致，否则同一份记忆的可检索性会随进程重启变化，BM25 的 docLen
// 也跟着漂。这条测试就是钉住「写完立刻能按 session 查到」。
func TestSessionSearchableRightAfterWrite(t *testing.T) {
	s, _ := newTestServer(t, `
[server]
data = "."

[[project]]
name = "notes"
owner = "alice"
mode = "0755"

[[user]]
name = "alice"
root = true
`)

	if _, err := s.ExecLineVars(context.Background(), "put --session 01a21b56-890b-759d $1", nil, []string{"可检索性"}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	hits := s.idxNow().Search("01a21b56-890b-759d", 5, 0)
	if len(hits) == 0 {
		t.Fatal("刚写完就按 session 查不到：写路径与重建路径索引的正文不一致")
	}
}
