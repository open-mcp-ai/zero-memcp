package server

import (
	"fmt"
	"os"
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
func (s *Server) maintain(done chan struct{}) {
	defer close(done)
	// 全局缺省：**不自动提交**。
	//
	// commit 的主路径是调用方（agent 插件）在上下文里总结完近期活动、
	// 写好 message 之后主动调。系统自己按钟点刷 commit，只会把
	// git 历史变成一堆无意义的 "batch of N"。
	//
	// 不提交不等于不安全：对象和脏层都已落盘（脏层还有延迟 fsync），
	// 重启时从脏层继续。代价只是「外部 git clone 看不到新内容」。
	defCommit := time.Duration(0)
	defGC := 24 * time.Hour
	defCount := 0
	// cfgFile 会被 Reload 在运行中替换，读它必须持锁。
	// 这里是唯一「读一份完整快照」的地方，所以整段在锁内取完再放。
	cf := s.cfgFilePtr.Load()
	if cf != nil {
		// 0 表示「不按这个条件触发」，必须被尊重，不能回落成默认值。
		//
		// 这条曾经是错的：`if d > 0 { defCommit = d }` 让显式写的 0
		// 被当成「没配」，于是回落成 30 秒——用户想关掉自动提交，
		// 配了 0，结果系统照样每 30 秒刷一次 commit。
		// 现在 [commit] 段缺省即 0，语义就是「完全由调用方手动 commit」。
		defCommit = cf.Commit.Interval
		defGC = cf.GC.Interval
		defCount = cf.Commit.Count
	}

	// 检查周期取所有仓库里最短的那个，保证每个仓库都能按时被处理。
	tick := time.NewTicker(s.shortestTick(defCommit))
	defer tick.Stop()

	states := &sync.Map{} // project -> *repoState
	now := time.Now()
	// 首次快照要早：启动后 60 秒就存一份，之后每 10 分钟。
	//
	// 为什么首次要这么急：Windows 上用 `taskkill /F` 停进程是硬杀，
	// Close() 根本不会执行——「退出前存快照」那条路在真实发版流程里
	// **不存在**。所以后台定时是唯一路径，它必须尽早跑一次，
	// 否则「下次重启就受益」永远等不到。
	//
	// 为什么之后放宽到 10 分钟：写快照是实打实的 I/O（压缩后几十 MB），
	// 而它的价值只体现在「下次启动」。
	var lastSnapAt = time.Now()
	snapDone := false
	// 脏层批量刷盘：每 2 秒把「已写未 fsync」的脏文件落盘一次。
	//
	// 为什么不每次写都刷：绝大多数崩溃是进程崩溃而非断电，页缓存还在。
	// 每写必刷会把单条写入从 0.5ms 拖到几毫秒，只换一个窄窗口的安全。
	// 2 秒是折中：断电最多丢 2 秒的写入，而 2 秒内攒的往往只有几条。
	lastSyncAt := time.Now()

	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			// 刷盘优先于一切：它是数据安全的底线，不受提交周期影响。
			if time.Since(lastSyncAt) >= 2*time.Second {
				s.syncDirtyAll()
				lastSyncAt = time.Now()
			}

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
				// 而外部 git clone 永远看不到新内容。让「另一个管理员
				// 来手动 commit」不现实——没人会为一个普通写入去担这个责任。
				//
				// 两条触发条件，任一满足就折叠：
				//   ① 条数达到 count（攒批：历史整洁，对象少）
				//   ② 距离上次提交超过 interval（时效：不让内容长期隐形）
				// 两个条件互相独立：0 只关掉它自己那一条。
				//
				// 早先写成 `interval > 0 && count > 0`，于是
				// 「按条数触发、不限时间」这种搭配根本不可能配出来——
				// 配了 count 也永远不触发，而配置里看不出任何异常。
				if interval > 0 || count > 0 {
					pending := repo.Pending()
					byCount := count > 0 && pending >= count
					byTime := interval > 0 && time.Since(st.lastCommit) >= interval
					if pending > 0 && (byCount || byTime) {
						if n, cerr := s.commitRepo(repo, proj, pending); cerr != nil {
							s.logf("%s automatic commit failed: %v", proj, cerr)
						} else if n > 0 {
							st.lastCommit = time.Now()
							// 提交成功即踢一次同步：这是「数据量先到」那一半触发。
							// 「2 小时先到」那一半由 startSyncLoop 的 ticker 兜底。
							s.kickSync()
						}
					}
				}

				// gc
				if gcEvery > 0 && time.Since(st.lastGC) >= gcEvery {
					if _, err := repo.Repack(); err != nil {
						s.logf("%s gc failed: %v", proj, err)
					}
					st.lastGC = time.Now()
				}
			}

			// 每天第一次有新写入时，在当时的 tip 上打一个锚点 tag。
			//
			// 「到 tag 为止」是召回侧遍历的边界：fileTimes 沿父链走时
			// 走到锚点就停，不必重走整个历史（实测 main.chat 8.29s → 0.24s）。
			// tag 会随 push 到远端，所以换机器、从备份恢复同样受益。
			//
			// 放到独立协程里，不占维护环。
			//
			// 为什么：给一条从没打过锚点的分支补第一个锚点，要沿父链
			// 走完全史算 FileTimes——实测 zero@inbox（2,074 个提交）
			// 44.3 秒。而维护环每秒跑一次，同一圈里还要刷脏层 fsync、
			// 折叠提交、存索引快照。卡在这里等于把那些一起停 44 秒，
			// 症状是「某次重启后写入延迟突然变成几十秒」。
			// 锚点晚几十秒打上没有任何影响——它只是让下次启动便宜。
			s.startAnchorWriter(projects)

			s.checkExternalUpdates()

			// 定期把索引快照落盘：下次启动就不必重扫重分词。
			//
			// 间隔 10 分钟：快照压缩后仍有几十 MB，写它是一次实打实的
			// I/O，而它的价值只体现在「下次启动」。10 分钟意味着
			// 「最多丢 10 分钟的索引变化」——那些变化在下次启动时
			// 会被 head 比对发现，然后走全量重建补上。
			//
			// 真正的时机是**退出时**（Close 里那一次）：正常发版
			// 就是「先停旧进程再起新的」，所以那一次才是主路径。
			snapEvery := 10 * time.Minute
			if !snapDone {
				snapEvery = 60 * time.Second
			}
			if s.indexSnapshotDue(lastSnapAt, snapEvery) {
				if err := s.saveIndexSnapshot(); err != nil {
					s.logf("index snapshot save failed: %v", err)
				} else {
					snapDone = true
				}
				lastSnapAt = time.Now()
			}
		}
	}
}

// indexSnapshotDue 判断是否该落一份新快照。
func (s *Server) indexSnapshotDue(last time.Time, every time.Duration) bool {
	if s.indexSnapshotDir() == "" {
		return false
	}
	return time.Since(last) >= every
}

// shortestTick 取一个合适的检查周期：不超过 1 秒，也不超过最短提交间隔的一半。
//
// 太粗会让「一改就提交」变成「几秒后才提交」，太细会空转。
func (s *Server) shortestTick(defCommit time.Duration) time.Duration {
	// 循环本身还负责刷盘、gc、索引快照、外部推进检测、语义追平。
	// 没有自动提交时这些事都是低频的，1 秒一次足够——不必 100ms 空转。
	const idleTick = time.Second

	t := defCommit
	if t <= 0 {
		t = idleTick
	}
	if cf := s.cfgFilePtr.Load(); cf != nil {
		for _, p := range s.declaredProjects() {
			if c := s.projects.MemcpConfig(p); c.CommitInterval > 0 && c.CommitInterval < t {
				t = c.CommitInterval
			}
		}
	}
	if t < 100*time.Millisecond {
		t = 100 * time.Millisecond
	}
	return t
}

// maybeWriteDailyCheckpoints 给「今天动过但还没打锚点」的分支打一个 tag。
//
// 时机选择：放在维护环里而不是写路径上。写路径是热路径（0.5ms），
// 而算一次完整 fileTimes 可能要几百毫秒到几秒——不能挂在 put 上。
// 维护环每秒跑一次，晚一秒打上完全够用。
//
// 判据是「分支头今天动过」，不是「本机有未提交写入」。
//
// 这里曾经用 PendingRefs()：只覆盖本机 put 过还没提交的分支。于是
// 同步进来的分支永远拿不到锚点——本机没有 pending，条件不成立。
// 实测代价（laptop，v0.2.11）：zero@inbox 有 2,074 个提交、0 个锚点，
// List() 每次沿父链走完整个历史要 62.4 秒，写一次锚点后 812ms。
// 它在启动路径上（Reload 扫描 + maintain 的 head 检测）被调两遍，
// 于是每次重启白等两分钟，日志上表现为「listing 前有几百秒空档」。
//
// 现在的判据从已提交的 head 来，同步、push、本机提交都算「动过」。
func (s *Server) maybeWriteDailyCheckpoints(projects []string) {
	today := time.Now().Format("2006-01-02")
	for _, proj := range projects {
		repo, err := s.projects.Get(proj)
		if err != nil {
			continue
		}
		for _, ref := range repo.Branches() {
			if repo.HasDailyCheckpoint(ref, today) {
				continue
			}
			// 头没动就别打：锚点的作用是把回溯截断在一个已知的 commit 上，
			// 而旧锚点已经指在同一个 head 时，新的那份数据完全一样。
			// 不判这一下，18 条分支每天各多一个 tag，而它们的数据一个字节没变。
			head, herr := repo.Resolve(ref)
			if herr != nil || head == "" {
				continue
			}
			if cp, cerr := repo.LatestDailyCheckpoint(ref); cerr == nil && cp != nil && cp.Commit == head {
				continue
			}
			// 打锚点要算出「此刻每个路径的最后改动时间」。
			// 已有旧锚点时这一步是增量的（只走旧锚点之后那一段）；
			// 从没锚点的分支则要第一次走完全史（inbox 实测 44 秒），
			// 那一次之后它就永远便宜了。
			times, ferr := repo.FileTimes(ref)
			if ferr != nil || len(times) == 0 {
				continue
			}
			if err := repo.WriteDailyCheckpoint(ref, today, "", times); err != nil {
				s.logf("daily checkpoint %s@%s: %v", proj, ref, err)
			}
		}
	}
}

// syncDirtyAll 把所有仓库的待刷工作层文件 fsync 落盘。
//
// 失败只记日志不中断：刷盘失败意味着磁盘有问题，但继续服务
// 仍然比整体停摆好——下一次刷盘会重试同一批文件。
func (s *Server) syncDirtyAll() {
	projects, err := s.projects.Projects()
	if err != nil {
		return
	}
	for _, proj := range projects {
		repo, err := s.projects.Get(proj)
		if err != nil {
			continue
		}
		if err := repo.SyncDirtyNow(); err != nil {
			s.logf("worklayer sync %s: %v", proj, err)
		}
	}
}

// commitBranch 在单个仓库的单个分支上执行提交，并把命令流归零到新树。
//
// 这是所有单分支提交的收敛处：execCommit / commitRepo / 树手术
// 都走这里，保证「产生新 commit」和「命令流归零」永远成对发生。
// 漏了后一半的后果不是数据错，而是命令流无限增长、对端基准永远对不上。
func (s *Server) commitBranch(proj string, repo *gitstore.Repo, branch, msg string) (string, error) {
	sha, err := repo.Commit(branch, msg)
	if err != nil {
		return "", err
	}
	// 提交把工作层的命令折进了树，命令流随即归零到新基准。
	s.resetOpLogToCommit(proj, branch, repo, sha)
	return sha, nil
}

// commitRepo 提交单个仓库的全部脏分支。
func (s *Server) commitRepo(repo *gitstore.Repo, proj string, n int) (int, error) {
	refs := repo.PendingRefs()
	committed := 0
	var firstErr error
	for _, ref := range refs {
		sha, err := s.commitBranch(proj, repo, ref, fmt.Sprintf("mem: %s batch of %d", proj, n))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		committed++
		if sha != "" {
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
	// 检查所有分支，不只 defaultBranch：受限凭证在自己分支上写东西、
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
		s.logf("branch head movement detected; refreshing the affected branches")
		s.publishExternalTreeChanges(current)

		// 只刷新真的动过的那几条分支。
		//
		// 这里曾经无条件 Reload()：一条分支推进就重扫全部仓库、
		// 重建整个倒排索引（21k 块、数十秒），期间整个服务不可用。
		// 而外部推进是常态（另一台机器同步、agent 写入后 commit），
		// 于是「偶发卡顿」变成了常态。
		byProject := map[string][]string{}
		for key, head := range current {
			if val, ok := s.lastCommits.Load(key); ok && val.(string) == head.Commit {
				continue
			}
			byProject[head.Project] = append(byProject[head.Project], head.Branch)
		}
		// 已删除的分支：它可能还有残留的树，交给全量重建处理。
		removed := false
		s.lastCommits.Range(func(k, _ any) bool {
			if _, exists := current[k.(string)]; !exists {
				removed = true
				return false
			}
			return true
		})
		if removed {
			s.logf("branch disappeared; falling back to a full reload")
			if err := s.Reload(); err != nil {
				s.logf("full reload failed: %v", err)
			}
			return
		}
		for project, branches := range byProject {
			s.refreshBranches(project, branches, "external head movement")
		}
		s.rememberTreeHeads()
	}
}

// logf 是带前缀的日志输出。
func (s *Server) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zero-memcp: "+format+"\n", args...)
}

// startAnchorWriter 在后台补锚点，不阻塞调用方（维护环）。
//
// 同时在跑的只允许一个：这条路径都是「把某条分支的历史走一遍」的重活，
// 并发跑多份只会互相抢磁盘，而锚点晚几十秒打上没有任何影响。
//
// 为什么不做成「每个分支一个协程」：那会在启动后瞬间扇出 18 个
// FileTimes 遍历，每一个都在读同一批 pack 文件——磁盘队列排满，
// 结果比串行还慢，而且内存里同时压 18 份 path→time 映射。
func (s *Server) startAnchorWriter(projects []string) {
	if len(projects) == 0 {
		return
	}
	run, ok := s.anchorWriter.start()
	if !ok {
		return // 已经有一个在跑，下一圈再补
	}
	// 调用方传进来的切片可能被复用，拷一份再带进协程。
	list := append([]string(nil), projects...)
	go func() {
		defer s.anchorWriter.finish(run)
		s.maybeWriteDailyCheckpoints(list)
	}()
}
