package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// read 的三种形态各钉一条。
//
// 起因（2026-11-28）：frontmatter 实测 95 字符，而 agent 侧取工具返回值的
// 前 120 个字符做摘要——read 原样吐整份文件时，摘要窗口里只有元数据，
// 正文一个字都没有。这一组钉的正是「摘要窗口里到底有没有正文」。
const sampleNote = "---\nsource: weft\nendpoint: laptop\ncreated: 2026-11-28T10:39:19+08:00\n---\n\n# 实时性\n\n正文第一行。\n"

func seedSampleNote(t *testing.T) (*Server, string) {
	t.Helper()
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "a.md", []byte(sampleNote), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}
	return srv, tok
}

func TestReadDefaultStripsFrontmatterAndKeepsProvenance(t *testing.T) {
	srv, tok := seedSampleNote(t)

	text, err := callMCP(t, srv, tok, "read blackegg/me@zero/a.md")
	if err != nil {
		t.Fatal(err)
	}

	if strings.HasPrefix(text, "---") {
		t.Fatalf("缺省形态不该以 frontmatter 开头（那正是它吃掉摘要窗口的原因）：\n%s", text)
	}
	if !strings.Contains(text, "正文第一行") {
		t.Fatalf("正文必须出现：\n%s", text)
	}
	// 溯源不能丢：写回时服务端会重新生成 created，原始时间戳只有这里能看到。
	if !strings.Contains(text, "source=weft") || !strings.Contains(text, "created=2026-11-28T10:39:19+08:00") {
		t.Fatalf("溯源必须压成一行保留：\n%s", text)
	}
	// 这是这个改动的全部意义：摘要窗口里要有正文。
	if !strings.Contains(truncate(text, 120), "实时性") {
		t.Fatalf("前 120 个字符里必须有正文，实际：%q", truncate(text, 120))
	}
}

func TestReadRawReturnsWholeFile(t *testing.T) {
	srv, tok := seedSampleNote(t)

	text, err := callMCP(t, srv, tok, "read blackegg/me@zero/a.md --raw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "---\nsource: weft") {
		t.Fatalf("--raw 必须原样给整份文件：\n%s", text)
	}
	// 返回文本末尾多一个换行（read 每篇后补一个），比较时忽略末尾空白。
	if strings.TrimRight(text, "\n") != strings.TrimRight(sampleNote, "\n") {
		t.Fatalf("--raw 必须字节精确。\n得到：%q\n期望：%q", text, sampleNote)
	}
}

func TestReadBareGivesOnlyBody(t *testing.T) {
	srv, tok := seedSampleNote(t)

	text, err := callMCP(t, srv, tok, "read blackegg/me@zero/a.md --bare")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "source=") || strings.Contains(text, "---") {
		t.Fatalf("--bare 不该留任何元数据：\n%s", text)
	}
	if !strings.Contains(text, "正文第一行") {
		t.Fatalf("--bare 必须给正文：\n%s", text)
	}
	// 多篇时 --bare 也不给 ==== 分隔：它要的就是「只有正文」。
	multi, err := callMCP(t, srv, tok, "read blackegg/me@zero/a.md blackegg/me@zero/a.md --bare")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(multi, "====") {
		t.Fatalf("--bare 不该有分隔装饰：\n%s", multi)
	}
}

// 两个选项互相矛盾时必须报错，不能猜。
func TestReadRawAndBareContradict(t *testing.T) {
	srv, tok := seedSampleNote(t)
	if _, err := callMCP(t, srv, tok, "read blackegg/me@zero/a.md --raw --bare"); err == nil {
		t.Fatal("--raw 与 --bare 同时给必须报错")
	}
}

// 没有 frontmatter 的文件不许被砍开头。
func TestRenderNoteLeavesPlainFileAlone(t *testing.T) {
	const plain = "随便一段没有元数据的正文\n第二行\n"
	if got := renderNote(plain, false); got != plain {
		t.Fatalf("没有 frontmatter 时不该改动内容：%q", got)
	}
	// 正文里出现 ---- 不该被当成结束标记。
	const tricky = "---\nsource: w\n---\n\n甲\n----\n乙\n"
	if got := renderNote(tricky, false); !strings.Contains(got, "----") || !strings.Contains(got, "乙") {
		t.Fatalf("正文里的 ---- 被误当成结束标记：%q", got)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
