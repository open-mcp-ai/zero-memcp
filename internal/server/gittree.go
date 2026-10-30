package server

import (
	"fmt"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/manifest"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// loadGitTrees 按分支扫描所有项目，构建内存树。
func (s *Server) loadGitTrees(persona string) ([]*memory.Tree, error) {
	projects, err := s.projects.Projects()
	if err != nil {
		return nil, err
	}

	var trees []*memory.Tree
	for _, proj := range projects {
		repo, err := s.projects.Get(proj)
		if err != nil {
			return nil, fmt.Errorf("打开项目 %s: %w", proj, err)
		}

		// 索引所有分支，不只 persona。
		//
		// 为什么必须这样：角色是按分支划分的（chat 写 daily@chat、
		// dev 写 daily@dev）。只索引 persona 的话，chat 角色写进
		// chat 分支的内容永远搜不到——写成功、read 读得到、
		// recall 无命中，一个不报错的静默失效。
		//
		// 索引是纯内存派生物，多扫几个分支的代价只是启动时多几百毫秒。
		branches := repo.Branches()
		if len(branches) == 0 {
			branches = []string{persona}
		}
		// 把 persona 和 pending 里的分支也带上（可能是还没提交的新分支）
		seen := map[string]bool{}
		for _, b := range branches {
			seen[b] = true
		}
		if !seen[persona] {
			branches = append(branches, persona)
			seen[persona] = true
		}
		for _, b := range repo.PendingRefs() {
			if !seen[b] {
				branches = append(branches, b)
				seen[b] = true
			}
		}

		for _, br := range branches {
			has, _ := repo.Resolve(br)
			if has == "" && repo.Pending(br) == 0 {
				continue
			}
			t, err := s.scanGitProject(repo, proj, br)
			if err != nil {
				return nil, fmt.Errorf("扫描 %s/%s: %w", proj, br, err)
			}
			trees = append(trees, t)
		}
	}
	return trees, nil
}

// scanGitProject 把一个仓库的一个分支读成内存树。
func (s *Server) scanGitProject(repo *gitstore.Repo, project, persona string) (*memory.Tree, error) {
	entries, err := repo.List(persona)
	if err != nil {
		return nil, err
	}

	t := &memory.Tree{Project: project, Branch: persona}
	for _, e := range entries {
		path := e.Path
		r := repo
		read := func() ([]byte, error) {
			return r.Read(persona, path)
		}

		block := memory.Block{
			Project: project,
			Branch:  persona,
			Path:    path,
			Size:    e.Size,
			Time:    e.Time,
			Read:    read,
		}

		// 以点开头的目录/文件（.legacy-archive、.githooks、.gitignore）
		// 是归档与工具，不是正文：保留可读，但不进检索。
		if memory.HiddenPath(path) {
			block.Hidden = true
			t.Blocks = append(t.Blocks, block)
			continue
		}

		// 规则与索引的判定，优先看文件自己写的 frontmatter。
		//
		// 为什么不再用 Git 的可执行位：100755 在真实仓库里的含义是
		// 「这是个可执行脚本」。用它当「规则」标记，后果是
		// .githooks/pre-push 和一个 11KB 的 python 脚本被当成规则
		// 注入系统提示，而真正的 SKILL.md（全是 100644）一个都没进。
		// 文件自己写着 inject: every_turn —— 那才是作者的本意。
		if strings.HasSuffix(path, ".md") {
			if data, rerr := read(); rerr == nil && hasFrontmatter(data) {
				pol := manifest.Parse(project, data)
				switch pol.Inject {
				case "every_turn":
					block.Rule = true
				case "never":
					block.Rule = false
				default:
					// 没声明 inject：说明作者只写了 write 策略，
					// 这时仍按 Git 的可执行位判断，保持旧行为。
					block.Rule = e.Mode == gitstore.ModeRule
				}
				if pol.Index == "none" {
					block.Hidden = true
				}
			} else {
				// 没有 frontmatter 的 Markdown：按老规矩看 mode 位
				block.Rule = e.Mode == gitstore.ModeRule
			}
		} else {
			// 非 Markdown 一律不是规则。
			// 可执行脚本再「可执行」也只是工具，不是给模型的指令。
			block.Rule = false
			if isExecutableScript(path) {
				block.Hidden = true
			}
		}

		t.Blocks = append(t.Blocks, block)
	}
	// 规则目录的兄弟文件也是规则。
	//
	// 判据来自 SKILL.md 自己写的那句「按文件名顺序读就是全文」——
	// 也就是说 01-rebuilt.md、02-shape.md … 和 SKILL.md 合起来才是
	// 一份完整的人格。只注入目录索引而不注入正文，等于给了目录
	// 却不给内容：模型知道"有 6 个文件"，但一个字都看不到。
	ruleDirs := map[string]bool{}
	for _, b := range t.Blocks {
		if b.Rule {
			if i := strings.LastIndex(b.Path, "/"); i > 0 {
				ruleDirs[b.Path[:i]] = true
			}
		}
	}
	if len(ruleDirs) > 0 {
		for i := range t.Blocks {
			b := &t.Blocks[i]
			if b.Rule || b.Hidden || !strings.HasSuffix(b.Path, ".md") {
				continue
			}
			dir := b.Path
			if j := strings.LastIndex(dir, "/"); j > 0 {
				dir = dir[:j]
			} else {
				dir = ""
			}
			if ruleDirs[dir] {
				b.Rule = true
			}
		}
	}

	t.Sort()
	return t, nil
}

// hasFrontmatter 判断文件是否以 YAML frontmatter 开头。
func hasFrontmatter(data []byte) bool {
	return strings.HasPrefix(string(data), "---")
}

// isExecutableScript 判断是不是脚本/工具类文件。
func isExecutableScript(path string) bool {
	for _, ext := range []string{".sh", ".py", ".bash", ".zsh", ".js", ".mjs", ".ts", ".rb", ".pl"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// addBlockLocked 把一条刚写入的切片加进内存树，让它毫秒级可搜。
//
// 调用方必须已持有 s.mu 写锁。
//
// 为什么不用 Reload()：Reload 要重扫全部仓库（2286 个文件、四万多次
// tree 解析）。写一条记忆触发一次全量重建，写入就会变成秒级操作，
// 而写入是热路径。
func (s *Server) addBlockLocked(project string, repo *gitstore.Repo, branch, path string, size int64, mtime int64) {
	if s.idx == nil {
		return
	}
	r := repo
	read := func() ([]byte, error) { return r.Read(branch, path) }

	// 找现有的树；没有就新建
	var tree *memory.Tree
	for _, t := range s.trees {
		if t.Project == project && t.Branch == branch {
			tree = t
			break
		}
	}
	if tree == nil {
		tree = &memory.Tree{Project: project, Branch: branch}
		s.trees = append(s.trees, tree)
	}

	// 同路径覆盖（同一毫秒内重写同一文件时不会产生重复条目）
	for i := range tree.Blocks {
		if tree.Blocks[i].Path == path {
			tree.Blocks[i].Size = size
			tree.Blocks[i].Time = mtime
			return
		}
	}

	tree.Blocks = append(tree.Blocks, memory.Block{
		Project: project,
		Branch:  branch,
		Path:    path,
		Size:    size,
		Time:    mtime,
		Read:    read,
	})
	tree.Sort()
}
