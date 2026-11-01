package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
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
	// cfgFile 会被 Reload 在运行中替换，读它必须持锁。
	// 这里是唯一「读一份完整快照」的地方，所以整段在锁内取完再放。
	s.cfgMu.RLock()
	cf := s.cfgFile
	s.cfgMu.RUnlock()
	if cf != nil {
		if d := cf.Commit.Interval; d > 0 {
			defCommit = d
		}
		if d := cf.GC.Interval; d > 0 {
			defGC = d
		}
		defCount = cf.Commit.Count
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

				// 提交策略：脏层不会永远留着。
				//
				// 为什么需要它：put 只投递，如果没人折叠，脏文件会无限增长，
				// 而外部 git clone 永远看不到新内容。让「另一个 root agent
				// 来手动 commit」不现实——没人会为一个普通写入去担这个责任。
				//
				// 两条触发条件，任一满足就折叠：
				//   ① 条数达到 count（攒批：历史整洁，对象少）
				//   ② 距离上次提交超过 interval（时效：不让内容长期隐形）
				if interval > 0 && count > 0 {
					pending := repo.Pending()
					if pending > 0 && (pending >= count || time.Since(st.lastCommit) >= interval) {
						if n, cerr := s.commitRepo(repo, proj, pending); cerr != nil {
							s.logf("%s 自动提交失败: %v", proj, cerr)
						} else if n > 0 {
							st.lastCommit = time.Now()
						}
					}
				}

				// gc
				if gcEvery > 0 && time.Since(st.lastGC) >= gcEvery {
					if _, err := repo.Repack(); err != nil {
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
	s.cfgMu.RLock()
	hasCfg := s.cfgFile != nil
	s.cfgMu.RUnlock()
	if hasCfg {
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

// commitRepo 提交单个仓库的全部脏分支。
func (s *Server) commitRepo(repo *gitstore.Repo, proj string, n int) (int, error) {
	refs := repo.PendingRefs()
	committed := 0
	var firstErr error
	for _, ref := range refs {
		if _, err := repo.Commit(ref, fmt.Sprintf("mem: %s batch of %d", proj, n)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		committed++
		if sha, err := repo.Resolve(ref); err == nil && sha != "" {
			s.lastCommits.Store(treeHeadKey(proj, ref), sha)
		}
	}
	return committed, firstErr
}

// checkExternalUpdates 检查裸仓库的 commit 是否被外部推进。
//
// 只要有一处变了就触发 Reload —— 另一台设备 push 上来、
// 或者是人手敲的 git commit，不需要人去叫它，也不用重启服务。
func (s *Server) checkExternalUpdates() {
	if s.cfg.DataDir == "" {
		return
	}
	// 检查所有分支，不只 persona：chat 在 daily@chat 写东西、
	// 或外部 git push 推进了某条分支，都应该被感知并触发 Reload。
	current := s.treeHeadsSnapshot()
	changed := false
	for key, head := range current {
		val, ok := s.lastCommits.Load(key)
		if !ok || val.(string) != head.Commit {
			changed = true
			break
		}
	}
	if !changed {
		s.lastCommits.Range(func(k, _ any) bool {
			if _, exists := current[k.(string)]; !exists {
				changed = true
				return false
			}
			return true
		})
	}
	if changed {
		s.logf("检测到分支头演进，自动重建索引并广播树差分")
		_ = s.Reload()
	}
}

// logf 是带前缀的日志输出。
func (s *Server) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zero-memcp: "+format+"\n", args...)
}

var _ = filepath.Join
