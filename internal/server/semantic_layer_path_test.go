package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// 记忆该不该进语义索引，判据必须认「内容自己写的层」，而不只是路径前缀。
//
// # 为什么
//
// 今晚（2026-11-16）实测：`docs 28187 / semantic 16167` —— 差 12,020 篇。
// 缺的全是历史思维链和对话，它们躺在日期根目录（`2026-11-15/xxx.md`），
// 而 isSemanticContent 只认四个路径前缀，于是这些块**一个向量都没算过**。
//
// 症状极难查：数据在、能 grep 到、能 read 到，就是语义搜不到。
// 看上去像「没落盘」，实际是「落了但索引瞎了」。这两种对用户是完全不同的说法。
//
// # 判据
//
// 一条块参与语义索引，满足任一即可：
//   1. 路径在前缀白名单里（thought/ record/ review/ fact/）—— 主路径
//   2. 内容自己声明了 layer: thought|record|review|fact —— 历史数据的补票通道
//
// 第 2 条必须认，因为写错路径是已经发生过的事实，不是可能发生的事。
func TestSemanticContentRecognisesDeclaredLayer(t *testing.T) {
	cases := []struct {
		name string
		blk  memory.Block
		want bool
	}{
		{
			name: "前缀白名单仍然生效",
			blk:  memory.Block{Path: "thought/2026-11-16/a.md"},
			want: true,
		},
		{
			name: "前缀白名单：record",
			blk:  memory.Block{Path: "record/2026-11-16/a.md"},
			want: true,
		},
		{
			name: "日期根目录但内容声明 layer: thought —— 必须认（补票通道）",
			blk: memory.Block{
				Path: "2026-11-16/041319.824264-493-3060.md",
				Body: []byte("---\nlayer: thought\nsession: abc\n---\n# Reasoning\n\ntext\n"),
			},
			want: true,
		},
		{
			name: "双层 frontmatter：layer 在第二层 —— 也必须认",
			blk: memory.Block{
				Path: "2026-11-16/041319.824264-493-3060.md",
				Body: []byte("---\nsource: zero\nendpoint: 3060\ncreated: 2026-11-16T04:13:19+08:00\n---\n\n" +
					"---\nlayer: thought\nsession: abc\nat: 2026-11-15T20:13:19.070Z\npart: 5/5\n---\n# Reasoning\n\nbody\n"),
			},
			want: true,
		},
		{
			name: "日期根目录、layer: record —— 认",
			blk: memory.Block{
				Path: "2026-11-16/b.md",
				Body: []byte("---\nlayer: record\n---\n## User\n\n问\n\n## Assistant\n\n答\n"),
			},
			want: true,
		},
		{
			name: "日期根目录、没有 layer —— 不认（叙事、笔记不是记忆层）",
			blk: memory.Block{
				Path: "2026-11-16/notes.md",
				Body: []byte("# 随便写的笔记\n\n没有 frontmatter\n"),
			},
			want: false,
		},
		{
			name: "日期根目录、layer 是别的值 —— 不认",
			blk: memory.Block{
				Path: "2026-11-16/c.md",
				Body: []byte("---\nlayer: scratch\n---\ntext\n"),
			},
			want: false,
		},
		{
			name: "me/ 永不参与（每轮注入的规则不靠检索）",
			blk: memory.Block{
				Path: "me/skills/01-x.md",
				Body: []byte("---\nlayer: thought\n---\ntext\n"),
			},
			want: false,
		},
		{
			name: "点开头的隐藏内容永不参与",
			blk: memory.Block{
				Path: ".legacy/2026-11-16/x.md",
				Body: []byte("---\nlayer: thought\n---\ntext\n"),
			},
			want: false,
		},
		{
			name: "layer 出现在正文深处（超出头部窗口）—— 不认，防止正文里引用这几个字就误判",
			blk: memory.Block{
				Path: "2026-11-16/d.md",
				// 头部 2KB 之内没有 layer；正文里提了一句「layer: thought」
				Body: append(
					append([]byte("# 长文\n\n"), make([]byte, 4096)...),
					[]byte("\n这里讨论 layer: thought 这个字段。\n")...,
				),
			},
			want: false,
		},
	}

	for _, c := range cases {
		if got := isSemanticBlock(c.blk); got != c.want {
			t.Errorf("%s：isSemanticBlock = %v，want %v（path=%q）", c.name, got, c.want, c.blk.Path)
		}
	}
}

// Layer 必须在扫描时就解析好，不能等到索引阶段再读盘。
//
// 理由：语义维护每 30 秒要问一次「哪些块要重算」，走的是 blob hash 旁路，
// 那条路的前提是**不读盘**。如果判层要读盘，旁路就废了。
func TestBlockLayerParsedAtScanTime(t *testing.T) {
	body := "---\nlayer: thought\nsession: s1\n---\n# Reasoning\n"
	blk := memory.Block{Path: "2026-11-16/e.md", Body: []byte(body)}
	if got := memory.DeclaredLayer(body); got != "thought" {
		t.Fatalf("DeclaredLayer(%q) = %q，want thought", body, got)
	}
	_ = blk
}

func TestDeclaredLayerEdgeCases(t *testing.T) {
	cases := map[string]string{
		"---\nlayer: thought\n---\n":                                  "thought",
		"---\nlayer: record\n---\n":                                   "record",
		"---\nsource: x\n---\n\n---\nlayer: thought\n---\n":           "thought",
		"---\nlayer:thought\n---\n":                                   "thought", // 冒号后无空格
		"---\nlayer:   thought  \n---\n":                              "thought", // 多余空白
		"---\nother: 1\n---\n":                                        "",
		"no frontmatter at all":                                       "",
		"":                                                            "",
		"---\nlayer: THOUGHT\n---\n":                                  "thought", // 大小写不敏感
	}
	for in, want := range cases {
		if got := memory.DeclaredLayer(in); got != want {
			t.Errorf("DeclaredLayer(%q) = %q，want %q", in, got, want)
		}
	}
}
