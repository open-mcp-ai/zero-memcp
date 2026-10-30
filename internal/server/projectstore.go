package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// ProjectStore 管理所有裸仓库。
//
// 对应关系：
//   - 配置文件里的 [[project]] 声明了每个项目的名字、属主和 mode
//   - 磁盘上对应的目录是 <data>/repos/<name>.git
//   - 每个人格对应仓库里的一个分支（refs/heads/<persona>）
type ProjectStore struct {
	dataDir string

	mu    sync.RWMutex
	repos map[string]*gitstore.Repo
}

// NewProjectStore 初始化仓库管理器。
func NewProjectStore(dataDir string) *ProjectStore {
	return &ProjectStore{
		dataDir: dataDir,
		repos:   map[string]*gitstore.Repo{},
	}
}

// RepoDir 返回某个项目的裸仓库路径。
func (ps *ProjectStore) RepoDir(project string) string {
	return filepath.Join(ps.dataDir, "repos", project+".git")
}

// Get 获取某个项目的仓库句柄，必要时打开它。
//
// 目录不存在时不自动创建——「项目存在」是一个需要明确声明的事，
// 不该由一次读取顺带造就。
func (ps *ProjectStore) Get(project string) (*gitstore.Repo, error) {
	ps.mu.RLock()
	r, ok := ps.repos[project]
	ps.mu.RUnlock()
	if ok {
		return r, nil
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.repos[project]; ok {
		return r, nil
	}

	dir := ps.RepoDir(project)
	r, err := gitstore.Open(dir)
	if err != nil {
		return nil, err
	}
	ps.repos[project] = r
	return r, nil
}

// Ensure 保证某个项目存在（不存在则自动 git init --bare）。
func (ps *ProjectStore) Ensure(project string) (*gitstore.Repo, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if r, ok := ps.repos[project]; ok {
		return r, nil
	}

	dir := ps.RepoDir(project)
	var r *gitstore.Repo
	var err error
	if _, statErr := os.Stat(dir); statErr == nil {
		r, err = gitstore.Open(dir)
	} else {
		r, err = gitstore.Init(dir)
	}
	if err != nil {
		return nil, err
	}
	ps.repos[project] = r
	return r, nil
}

// Projects 递归列出全部已存在的项目名，按字典序。
//
// 项目名就是裸仓库目录名去掉 .git，相对于 repos/ 的路径。
// 单层（me.git → "me"）和多层（blackegg/me.git → "blackegg/me"）
// 一视同仁——层级多少是使用者的事，不是代码的假设。
func (ps *ProjectStore) Projects() ([]string, error) {
	reposDir := filepath.Join(ps.dataDir, "repos")
	var out []string

	err := filepath.WalkDir(reposDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == ".git" || strings.HasSuffix(name, ".git") {
			if name == ".git" {
				return filepath.SkipDir
			}
			rel, rerr := filepath.Rel(reposDir, path)
			if rerr == nil {
				out = append(out, filepath.ToSlash(strings.TrimSuffix(rel, ".git")))
			}
			// 裸仓库内部不再往下走
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// CommitAll 把所有仓库里攒着的写入落成提交。
// 返回提交成功的项目数。
func (ps *ProjectStore) CommitAll(ref, message string) (int, error) {
	ps.mu.RLock()
	var repos []*gitstore.Repo
	for _, r := range ps.repos {
		repos = append(repos, r)
	}
	ps.mu.RUnlock()

	committed := 0
	for _, r := range repos {
		if r.Pending() == 0 {
			continue
		}
		if _, err := r.Commit(ref, message); err != nil {
			return committed, fmt.Errorf("%s: %w", r.Path, err)
		}
		committed++
	}
	return committed, nil
}

// MemcpConfig 返回某个项目的仓库专属策略。
//
// 项目不存在时返回零值——零值意味着「全部用全局缺省」，
// 所以取不到配置不会导致某个仓库停止提交或不再 gc。
func (ps *ProjectStore) MemcpConfig(project string) gitstore.MemcpConfig {
	r, err := ps.Get(project)
	if err != nil {
		return gitstore.MemcpConfig{}
	}
	return r.MemcpConfig()
}
