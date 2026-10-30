package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// repoState 是单个仓库的维护状态。
//
// 待提交条数不在这里记——那是 gitstore 的事实（Repo.Pending()）。
// 两边各记一份的后果是它们迟早对不上，然后提交时机变得无法解释。
type repoState struct {
	// lastCommit 是上次提交的时间，用来判断是否到了该仓库的间隔。
	lastCommit time.Time
	// lastGC 是上次 gc 的时间。
	lastGC time.Time
}

// maintain 是后台维护循环。
//
// 周期来自两层：
//  1. 全局缺省：zero-memcp.toml 的 [commit] 与 [gc]
//  2. 仓库覆盖：<repo>.git/config 的 [memcp]（配了就以它为准）
//
// 为什么必须支持仓库级：宪法仓库希望「一改就提交」（留痕细），
// 流水记忆仓库希望「攒一批再提交」（历史整洁）。同一套周期伺候不了两者。
func (s *Server) maintain() {
	// 全局缺省
	defCommit := 30 * time.Second
	defGC := 24 * time.Hour
	defCount := 0
	if s.cfgFile != nil {
		if d := s.cfgFile.Commit.Interval; d > 0 {
			defCommit = d
		}
		if d := s.cfgFile.GC.Interval; d > 0 {
			defGC = d
		}
		defCount = s.cfgFile.Commit.Count
	}

	// 检查周期取所有仓库里最短的那个，保证每个仓库都能按时被处理。
	tick := time.NewTicker(s.shortestTick(defCommit))
	defer tick.Stop()

	states := &sync.Map{} // project -> *repoState
	now := time.Now()

	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			projects, err := s.projects.Projects()
			if err != nil {
				continue
			}

			// 有仓库的周期被改短了，就跟着加快检查频率
			tick.Reset(s.shortestTick(defCommit))

			for _, proj := range projects {
				repo, err := s.projects.Get(proj)
				if err != nil {
					continue
				}

				v, _ := states.LoadOrStore(proj, &repoState{lastCommit: now, lastGC: now})
				st := v.(*repoState)

				// 仓库级覆盖优先
				rc := repo.MemcpConfig()
				interval := defCommit
				if rc.CommitInterval > 0 {
					interval = rc.CommitInterval
				}
				count := defCount
				if rc.CommitCount > 0 {
					count = rc.CommitCount
				}
				gcEvery := defGC
				if rc.GCInterval > 0 {
					gcEvery = rc.GCInterval
				}

				// 自动提交已彻底移除：
				//
				// 封箱时机属于语义判断，由 Agent（或外部显式调用 commit）控制。
				// 引擎硬编码「攒够 50 条或等 30 秒」会在多分支和审计场景下打乱
				// 边界——审计 Agent 刚要比对，后台定时器把分支往前推了，
				// 造成未审和已审错位。
				//
				// WAL 保障断电不丢一条写入，所以不自动提交也是完全安全的。
				_ = count
				_ = interval

				// gc
				if gcEvery > 0 && time.Since(st.lastGC) >= gcEvery {
					if err := runGitGC(repo.Path); err != nil {
						s.logf("%s gc 失败: %v", proj, err)
					}
					st.lastGC = time.Now()
				}
			}

			s.checkExternalUpdates()
		}
	}
}

// shortestTick 取一个合适的检查周期：不超过 1 秒，也不超过最短提交间隔的一半。
//
// 太粗会让「一改就提交」变成「几秒后才提交」，太细会空转。
func (s *Server) shortestTick(defCommit time.Duration) time.Duration {
	t := defCommit
	if s.cfgFile != nil {
		for _, p := range s.declaredProjects() {
			if c := s.projects.MemcpConfig(p); c.CommitInterval > 0 && c.CommitInterval < t {
				t = c.CommitInterval
			}
		}
	}
	if t > time.Second {
		t = time.Second
	}
	if t < 100*time.Millisecond {
		t = 100 * time.Millisecond
	}
	return t
}

// commitRepo 提交单个仓库。
func (s *Server) commitRepo(repo interface {
	Pending(ref ...string) int
	Commit(ref, message string) (string, error)
}, proj string, n int) (int, error) {
	if repo.Pending() == 0 {
		return 0, nil
	}
	persona := s.persona
	if persona == "" {
		persona = "zero"
	}
	if _, err := repo.Commit(persona, fmt.Sprintf("mem: %s batch of %d", proj, n)); err != nil {
		return 0, err
	}
	return 1, nil
}

// flushPending 立刻提交所有攒着的写入。用于测试与关闭前。
func (s *Server) flushPending() {
	persona := s.persona
	if persona == "" {
		persona = "zero"
	}
	n, err := s.projects.CommitAll(persona, "mem: flush")
	if err != nil {
		s.logf("提交失败: %v", err)
		return
	}
	if n > 0 {
		s.logf("已提交 %d 个项目", n)
	}
}

// notePending 记录一次待提交的写入。
//
// 这里只负责让写入「被记住」，真正什么时候提交由 maintain 按
// 每个仓库自己的周期和条数决定。
func (s *Server) notePending() {
	s.pendingMu.Lock()
	s.pending++
	s.pendingMu.Unlock()
}

// gcAll 对所有裸仓库执行 git gc。递归发现多层项目目录。
func (s *Server) gcAll() {
	projects, err := s.projects.Projects()
	if err != nil {
		return
	}
	for _, p := range projects {
		r, err := s.projects.Get(p)
		if err != nil {
			continue
		}
		if err := runGitGC(r.Path); err != nil {
			s.logf("gc %s 失败: %v", p, err)
		}
	}
}

// runGitGC 在一个裸仓库里执行 git gc --prune=now。
//
// --prune=now 而不是默认的两周宽限：默认宽限是为了让误删的提交能被找回，
// 但我们的悬空对象全是「写进磁盘但还没提交」的残留，它们永远不会有 ref
// 指向，留着只是占地方。
func runGitGC(repo string) error {
	cmd := exec.Command("git", "-C", repo, "gc", "--prune=now", "--quiet")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// checkExternalUpdates 检查裸仓库的 commit 是否被外部推进。
//
// 只要有一处变了就触发 Reload —— 另一台设备 push 上来、
// 或者是人手敲的 git commit，不需要人去叫它，也不用重启服务。
func (s *Server) checkExternalUpdates() {
	if s.cfg.DataDir == "" {
		return
	}
	persona := s.persona
	if persona == "" {
		persona = "zero"
	}
	projs, err := s.projects.Projects()
	if err != nil {
		return
	}
	changed := false
	for _, p := range projs {
		r, err := s.projects.Get(p)
		if err != nil {
			continue
		}
		commit, _ := r.Resolve(persona)
		if commit == "" {
			continue
		}
		// 基线在 Reload 时就记下了，这里只做比较。
		//
		// 曾经的 bug：基线在第一次 tick 才记录。如果外部提交发生在
		// 启动到第一次 tick 之间，那次提交会被当成基线吸收掉，
		// 永远不会被识别为「变化」——索引从此跟仓库不一致，且不报错。
		key := p + "/" + persona
		if val, ok := s.lastCommits.Load(key); ok && val.(string) != commit {
			changed = true
		}
	}
	if changed {
		s.logf("检测到外部提交，自动重建索引")
		_ = s.Reload()
	}
}

// logf 是带前缀的日志输出。
func (s *Server) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zero-memcp: "+format+"\n", args...)
}

var _ = filepath.Join
