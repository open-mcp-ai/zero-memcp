package server

import (
	"errors"
	"fmt"
	"io/fs"
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
//   - 每条版本线对应仓库里的一个分支（refs/heads/<name>）
type ProjectStore struct {
	dataDir string
	// slots 是 project -> *repoSlot。用 sync.Map 而不是 map+Mutex：
	// 这里没有跨 key 的不变量，每次操作只关心一个 project。
	slots sync.Map
}

// repoSlot 是一个项目的「单飞」占位：第一个到的人负责打开仓库，
// 其余人等 done 关闭后直接取结果。
//
// 为什么不用锁：打开仓库是磁盘 I/O（stat、读 config、建 pack 索引）。
// 把 I/O 放进互斥锁，遇到慢盘/网络盘就变成全站排队；
// 而单飞只让**同一个 project** 的并发调用者等待，不同 project 互不影响，
// 且等待的是 close(done) —— 有明确终点，不像 Lock() 那样没有上界。
type repoSlot struct {
	done chan struct{}
	repo *gitstore.Repo
	err  error
}

// acquire 返回该 project 的仓库；不存在时由第一个调用者用 open 创建。
//
// open 会在**没有任何共享状态被保护**的情况下执行 —— 它只属于这一次调用。
func (ps *ProjectStore) acquire(project string, open func(dir string) (*gitstore.Repo, error)) (*gitstore.Repo, error) {
	fresh := &repoSlot{done: make(chan struct{})}
	v, loaded := ps.slots.LoadOrStore(project, fresh)
	if loaded {
		sl := v.(*repoSlot)
		<-sl.done
		return sl.repo, sl.err
	}

	// 我是第一个：负责打开，然后关掉 done 让后来者经过。
	repo, err := open(ps.RepoDir(project))
	fresh.repo, fresh.err = repo, err
	close(fresh.done)
	if err != nil {
		// 失败不留下毒化的 slot：下次调用可以重试。
		// 否则一次磁盘抖动会把那个 project 永久钉死在错误上。
		ps.slots.CompareAndDelete(project, fresh)
	}
	return repo, err
}

// each 遍历已打开的仓库，不阻塞任何打开动作。
func (ps *ProjectStore) each(fn func(*gitstore.Repo)) {
	ps.slots.Range(func(k, v any) bool {
		sl := v.(*repoSlot)
		select {
		case <-sl.done:
			if sl.repo != nil {
				fn(sl.repo)
			}
		default:
			// 还在打开中，跳过。这些仓库还没有被任何请求真正用起来。
		}
		return true
	})
}

// NewProjectStore 初始化仓库管理器。
func NewProjectStore(dataDir string) *ProjectStore {
	return &ProjectStore{dataDir: dataDir}
}

// RepoDir 返回某个项目的裸仓库路径。
func (ps *ProjectStore) RepoDir(project string) string {
	return filepath.Join(ps.dataDir, "repos", project+".git")
}

// Close 停止所有已经打开的仓库写者。
//
// ProjectStore 关闭后不再拥有活动请求，逐个关闭 Repo 会等待已接收的
// 脏写任务排空，避免进程退出时留下后台 goroutine 或正在进行的文件写入。
func (ps *ProjectStore) Close() error {
	var repos []*gitstore.Repo
	ps.each(func(r *gitstore.Repo) { repos = append(repos, r) })

	var firstErr error
	for _, repo := range repos {
		if err := repo.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Get 获取某个项目的仓库句柄，必要时打开它。
//
// 目录不存在时不自动创建——「项目存在」是一个需要明确声明的事，
// 不该由一次读取顺带造就。写入路径用 Ensure。
func (ps *ProjectStore) Get(project string) (*gitstore.Repo, error) {
	r, err := ps.acquire(project, gitstore.Open)
	if err != nil {
		return nil, openError(project, err)
	}
	return r, nil
}

// openError turns a repository open failure into something a caller may see.
//
// gitstore reports absolute paths because that is what an operator needs in a
// log. Callers of this server only ever see virtual addresses: a leaked
// "/var/lib/zero-memcp/data/repos/x.git" breaks that model and tells an
// unprivileged credential how the host is laid out. The underlying error stays
// wrapped for logs, but no filesystem path is rendered here.
func openError(project string, err error) error {
	switch {
	// errors.Is, not os.IsNotExist: gitstore wraps the stat failure in
	// fmt.Errorf("repository %s: %w", ...), and os.IsNotExist only unwraps the
	// os.PathError chain, not %w. With os.IsNotExist every missing repository
	// fell through to the generic branch and re-exported the absolute path.
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("project %q has no repository yet", project)
	case strings.Contains(err.Error(), "is not a bare repository"):
		return fmt.Errorf("project %q is not a bare repository", project)
	default:
		return fmt.Errorf("project %q cannot be opened: %w", project, err)
	}
}

// Ensure 保证某个项目存在（不存在则自动 git init --bare）。
func (ps *ProjectStore) Ensure(project string) (*gitstore.Repo, error) {
	r, err := ps.acquire(project, func(dir string) (*gitstore.Repo, error) {
		if _, statErr := os.Stat(dir); statErr == nil {
			return gitstore.Open(dir)
		}
		return gitstore.Init(dir)
	})
	if err != nil {
		return nil, openError(project, err)
	}
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
	var repos []*gitstore.Repo
	ps.each(func(r *gitstore.Repo) { repos = append(repos, r) })

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
