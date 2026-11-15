package server

import (
	"fmt"
	"sort"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)


// callMCPVars 用一个命名变量传正文——多行 frontmatter 必须走这条路。
// （`-- <正文>` 的 legacy 写法会把换行吃掉，且 termcp 只认单一 payload 引用。）
func callMCPVars(t *testing.T, srv *Server, tok, addr, body string) (string, error) {
	t.Helper()
	return callMCPRaw(t, srv, tok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write " + addr + " $BODY",
			"vars": map[string]string{"BODY": body},
		},
	})
}

// 对拍：增量刷新（refreshPath）跑完，结果必须与全量重建（refreshBranch）
// 逐字段相同。
//
// 为什么必须有这条测试：增量改造的风险不是「慢」，是「少做了一步」——
// 而少做的那一步对所有人静默失效，不报错。
//   - 只 AddWithTime 内容、漏掉 Rule/Hidden 重新分类
//     → 改一下 frontmatter，那条规则从此不注入，谁也不报错
//   - 漏掉 SKILL.md 的 policy 重解析
//     → 内容对、派生旧，权限判定用的是上一版
//   - 新文件在 tree.Blocks 里没有对应条目时被实现成 no-op
//     → 写成功了，但搜不到
//
// 这三类都没有症状，只有副作用。所以判据必须是「两条路结果完全一样」，
// 而不是「我想到的派生项刚好是全集的」。
//
// 覆盖的变更类型：新增 / 改内容 / 改 frontmatter / 删除。
func TestRefreshPathMatchesFullRebuild(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const (
		proj = "blackegg/me"
		br   = "fact"
	)

	// 先铺一批底料，让树里有多样形态的块。
	seeds := map[string]string{
		"plain.md":            "普通正文，无 frontmatter",
		"frontmatter-idx.md":  "---\ninject: every_turn\n---\n\n这条声明了每轮注入",
		"frontmatter-none.md": "---\nindex: none\n---\n\n这条声明不进检索",
		"frontmatter-plain.md": "---\nwrite: append\n---\n\n只写了 write 策略",
		"notes/a.md":          "子目录里的普通文件",
		"notes/b.md":          "另一个子目录文件",
	}
	for p, body := range seeds {
		if _, err := callMCPVars(t, srv, tok, proj+"@"+br+":"+p, body); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	// snapshot 取出「索引 + 树」的可比较投影。
	//
	// 比的东西必须覆盖所有派生项，否则对拍本身会漏掉它该抓的东西：
	//   - 树里的 Block：(path, size, rule, hidden)
	//   - 索引里的可检索性：用 grep -c 数命中块数做代理
	//     （索引是倒排表，外部只能通过检索观察它）
	snapshot := func(t *testing.T) string {
		t.Helper()
		srv.mu.RLock()
		var view []string
		for _, tr := range srv.trees {
			if tr.Project != proj || tr.Branch != br {
				continue
			}
			for _, b := range tr.Blocks {
				view = append(view, fmt.Sprintf("%s|%d|rule=%v|hidden=%v",
					b.Path, b.Size, b.Rule, b.Hidden))
			}
		}
		srv.mu.RUnlock()
		sort.Strings(view)
		return fmt.Sprint(view)
	}

	// searchableView 用检索观察索引：哪些 path 能被一个通用词命中。
	// 索引坏了（该加的没加、该删的没删）这里就会变。
	searchableView := func(t *testing.T) string {
		t.Helper()
		out, err := callMCP(t, srv, tok, "grep 正文 --scope "+proj+"@"+br)
		if err != nil {
			t.Fatalf("grep: %v", err)
		}
		count, err := callMCP(t, srv, tok, "grep -c 正文 --scope "+proj+"@"+br)
		if err != nil {
			t.Fatalf("grep -c: %v", err)
		}
		return out + "\x00" + count
	}

	type op struct {
		name string
		path string
		run  func() error
	}
	ops := []op{
		{
			name: "新增文件",
			path: "newfile.md",
			run: func() error {
				_, err := callMCPVars(t, srv, tok, proj+"@"+br+":newfile.md", "新加的一条正文")
				return err
			},
		},
		{
			name: "改内容（不动 frontmatter）",
			path: "frontmatter-idx.md",
			run: func() error {
				_, err := callMCPVars(t, srv, tok, proj+"@"+br+":frontmatter-idx.md", "---\ninject: every_turn\n---\n\n改了正文但 inject 没变")
				return err
			},
		},
		{
			name: "改 frontmatter：普通 → every_turn（Rule 必须变 true）",
			path: "plain.md",
			run: func() error {
				_, err := callMCPVars(t, srv, tok, proj+"@"+br+":plain.md", "---\ninject: every_turn\n---\n\n现在这是规则了")
				return err
			},
		},
		{
			name: "改 frontmatter：every_turn → never（Rule 必须变 false）",
			path: "frontmatter-idx.md",
			run: func() error {
				_, err := callMCPVars(t, srv, tok, proj+"@"+br+":frontmatter-idx.md", "---\ninject: never\n---\n\n不再注入了")
				return err
			},
		},
		{
			name: "改 frontmatter：加上 index: none（Hidden 必须变 true）",
			path: "frontmatter-plain.md",
			run: func() error {
				_, err := callMCPVars(t, srv, tok, proj+"@"+br+":frontmatter-plain.md", "---\nindex: none\n---\n\n不进检索了")
				return err
			},
		},
		{
			name: "改 frontmatter：去掉 index: none（Hidden 必须变回 false）",
			path: "frontmatter-none.md",
			run: func() error {
				_, err := callMCPVars(t, srv, tok, proj+"@"+br+":frontmatter-none.md", "又进检索了")
				return err
			},
		},
		{
			name: "删除文件",
			path: "notes/b.md",
			run: func() error {
				_, err := callMCP(t, srv, tok, "delete "+proj+"@"+br+":notes/b.md")
				return err
			},
		},
		{
			name: "子目录新增",
			path: "notes/c.md",
			run: func() error {
				_, err := callMCPVars(t, srv, tok, proj+"@"+br+":notes/c.md", "子目录新文件")
				return err
			},
		},
	}

	for _, o := range ops {
		t.Run(o.name, func(t *testing.T) {
			// ① 用动词走增量路径（write/delete 内部就是 refreshPath）
			if err := o.run(); err != nil {
				t.Fatalf("%s: %v", o.name, err)
			}
			incTree, incSearch := snapshot(t), searchableView(t)

			// ② 对同一条分支做全量重建，得到参照
			if err := srv.refreshBranch(proj, br); err != nil {
				t.Fatalf("full rebuild: %v", err)
			}
			fullTree, fullSearch := snapshot(t), searchableView(t)

			// ③ 逐字节比
			if incTree != fullTree {
				t.Fatalf("%s：增量与全量重建的树不一致\n增量: %s\n全量: %s",
					o.name, incTree, fullTree)
			}
			if incSearch != fullSearch {
				t.Fatalf("%s：增量与全量重建的检索视图不一致\n增量: %q\n全量: %q",
					o.name, incSearch, fullSearch)
			}
		})
	}
}

// SKILL.md 是派生状态最典型的坑：内容改了 → policy 必须重解析。
// 早先 refreshPath 若不显式处理它，症状会是「内容更新了、权限还是旧的」。
func TestRefreshPathReparsesSkillPolicy(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const (
		proj = "blackegg/me"
		br   = "fact"
	)

	if _, err := callMCPVars(t, srv, tok, proj+"@"+br+":SKILL.md",
		"---\ninject: every_turn\n---\n\n版本一"); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	got, ok := srv.policies.Load(proj)
	if !ok {
		t.Fatal("写 SKILL.md 之后没有 policy")
	}
	if fmt.Sprint(got) == "" {
		t.Fatal("policy 是空的")
	}

	// 换一版 SKILL.md：policy 必须跟着变，不能还是上一份。
	if _, err := callMCPVars(t, srv, tok, proj+"@"+br+":SKILL.md",
		"---\ninject: never\n---\n\n版本二，不注入了"); err != nil {
		t.Fatalf("rewrite SKILL.md: %v", err)
	}
	got2, ok := srv.policies.Load(proj)
	if !ok {
		t.Fatal("改 SKILL.md 之后 policy 丢了")
	}
	if fmt.Sprint(got) == fmt.Sprint(got2) {
		t.Fatalf("SKILL.md 改了，policy 没跟着重解析：%v", got2)
	}
}

// 新文件第一次出现时，tree.Blocks 里没有对应条目。
// 如果增量实现成「只更新已有的」，这里会静默 no-op：写成功但搜不到。
func TestRefreshPathAddsBrandNewFile(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const (
		proj  = "blackegg/me"
		br    = "fact"
		path  = "brand-new/deep/file.md"
		marker = "zzz-brand-new-marker-zzz"
	)

	if _, err := callMCPVars(t, srv, tok, proj+"@"+br+":"+path, marker); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 树里必须有这个 Block。
	srv.mu.RLock()
	found := false
	for _, tr := range srv.trees {
		if tr.Project != proj || tr.Branch != br {
			continue
		}
		for _, b := range tr.Blocks {
			if b.Path == path {
				found = true
			}
		}
	}
	srv.mu.RUnlock()
	if !found {
		t.Fatalf("新文件 %s 没有进 tree.Blocks —— 增量路径对「树里还没有的 path」是 no-op", path)
	}

	// 索引里也必须能搜到。
	out, err := callMCP(t, srv, tok, "grep "+marker+" --scope "+proj+"@"+br)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !contains(out, marker) {
		t.Fatalf("新文件搜不到：%s", out)
	}

	_ = gitstore.ModeContent
	_ = memory.MakeID
}