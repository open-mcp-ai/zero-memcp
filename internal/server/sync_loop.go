package server

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 对等同步：把「fetch → merge → push」编排成一个后台循环。
//
// # 为什么是星型而不是网状
//
// 两台机器不保证同时在线。直连同步要求两边都活着，而星型（GitHub 作中枢）
// 只要求各自连得上中枢。中枢还顺带提供了异地备份和完整时间机器——
// 记忆是长期资产，不该只活在两台可能同时挂掉的机器上。
//
// # 每个周期做什么
//
//	1. fetch  <project> <remote>       取回远端所有分支到 refs/remotes/<remote>/*
//	2. 对每条远端跟踪分支：
//	     merge <project> <branch> from <remote>
//	       up-to-date / ahead → 无事可做
//	       fast-forward       → 指针推进
//	       merged             → 产生两亲提交
//	       conflict           → 记入队列，不推、不猜
//	3. push 领先的分支
//
// # 三条铁律（与手动动词同源）
//
//   - 不隐式提交：有脏层就跳过这个项目的同步，如实报数量
//   - 不强推：只接受快进，历史改写一律拒绝
//   - 冲突不静默：进队列、进状态输出、常驻直到人工处理
//
// # weights 不参与
//
// 权重衡量的是「这台机器上的使用习惯」，是派生状态，删掉都能重建。
// 它不进仓库，也不走同步通道——同步只搬真相（Git 对象），
// 不搬本机的阅读热度。

// syncConflict 是一条待人工决议的合并冲突。
type syncConflict struct {
	Project string
	Branch  string
	Remote  string
	Paths   []string
	At      time.Time
}

// syncState 是同步循环的进程内状态。
type syncState struct {
	mu sync.Mutex
	// conflicts 是待决议冲突，键为 project@branch。
	conflicts map[string]syncConflict
	// lastSync 记录每个 project@remote 的上次成功同步时间。
	lastSync map[string]time.Time
	// lastErr 记录每个 project@remote 的上次错误（成功即清）。
	lastErr map[string]string
	// stop 非 nil 表示循环在跑。
	stop chan struct{}
	kick chan struct{}
}

// syncOnce 对一条 auto_sync 远端执行一轮完整同步。
//
// 它不返回 error 给调用方决定流程：后台循环里单个项目的失败
// 不该影响其他项目，失败原因记进 state 供状态输出查。
func (s *Server) syncOnce(project string, rm config.Remote) {
	key := project + "@" + rm.Name
	root := s.rootCaller()

	repo, err := s.projects.Get(project)
	if err != nil {
		s.recordSyncErr(key, err.Error())
		return
	}

	// 铁律一：不隐式提交。有脏层就跳过整个项目——
	// 合并会推进分支指针，而脏层是相对旧指针的改动，
	// 在脏层未折叠时合并会让两边语义都不清楚。
	if pending := repo.Pending(); pending > 0 {
		s.recordSyncErr(key, fmt.Sprintf("%d uncommitted change(s); commit before syncing", pending))
		return
	}

	// ── 1. fetch ──
	if _, ferr := s.fetchRemote(root, project, rm, repo); ferr != nil {
		s.recordSyncErr(key, "fetch: "+ferr.Error())
		return
	}

	// ── 2. 逐分支 merge ──
	refs, err := repo.Refs("refs/remotes/" + rm.Name + "/")
	if err != nil {
		s.recordSyncErr(key, "list remote refs: "+err.Error())
		return
	}
	var toPush []string
	var conflicts []string
	for _, ref := range refs {
		branch := strings.TrimPrefix(ref, "refs/remotes/"+rm.Name+"/")
		if branch == "" || strings.Contains(branch, "/") {
			// 只处理一级分支名；带斜杠的是跟踪引用的子层级，跳过
			continue
		}
		res, merr := repo.MergeInto(branch, ref)
		if merr != nil {
			s.logf("sync %s: merge %s failed: %v", key, branch, merr)
			continue
		}
		switch res.Kind {
		case "ahead", "fast-forward", "merged":
			// ahead（本地新写的）、fast-forward（远端有更新本地快进）、
			// merged（两边各写了新内容、正交合并成功）：这三类都该推到远端，
			// 让中枢 GitHub 拿到最新真相。
			toPush = append(toPush, branch)
			s.logf("sync %s: %s %s (%s)", key, branch, res.Kind, shortSHA(res.Commit))
		case "conflict":
			conflicts = append(conflicts, branch)
			s.addSyncConflict(syncConflict{
				Project: project, Branch: branch, Remote: rm.Name,
				Paths: res.Conflicts, At: time.Now(),
			})
			s.logf("sync %s: %s CONFLICT on %d path(s): %s",
				key, branch, len(res.Conflicts), strings.Join(res.Conflicts, ", "))
		}
	}

	// ── 2.5 远端尚不存在的白名单分支（首次建分支）──
	//
	// GitHub 远端刚建时是空的，fetch 回来没有跟踪引用。
	// 如果只按 refs/remotes/<rm>/ 找，这些分支就永远不会被初次推上去。
	// 所以只要本地有、且在 rm.Push 白名单里、且远端还没有的，也要进 toPush。
	knownRemoteBranches := make(map[string]bool, len(refs))
	for _, ref := range refs {
		br := strings.TrimPrefix(ref, "refs/remotes/"+rm.Name+"/")
		knownRemoteBranches[br] = true
	}
	for _, br := range rm.Push {
		if br == "*" {
			continue
		}
		if !knownRemoteBranches[br] {
			if sha, err := repo.Resolve(br); err == nil && sha != "" {
				toPush = append(toPush, br)
				s.logf("sync %s: %s new branch on remote, will push initial commit %s", key, br, shortSHA(sha))
			}
		}
	}

	// ── 3. push 领先或新建的分支 ──
	pushFailed := 0
	pushed := make(map[string]bool)
	for _, branch := range toPush {
		if pushed[branch] || rm.FetchOnly || !remoteAllowsPush(rm, branch) {
			continue
		}
		pushed[branch] = true
		if _, err := s.pushOneBranch(root, project, rm, repo, branch); err != nil {
			pushFailed++
			s.logf("sync %s: push %s failed: %v", key, branch, err)
		}
	}

	// 冲突不算同步失败——它是需要人决议的常态。记下但仍更新成功时间，
	// 否则「有冲突」会让 lastSync 永远停住，看不出同步其实在跑。
	if len(conflicts) > 0 {
		s.recordSyncErr(key, fmt.Sprintf("%d branch(es) need manual resolution: %s",
			len(conflicts), strings.Join(conflicts, ", ")))
	} else if pushFailed > 0 {
		s.recordSyncErr(key, fmt.Sprintf("%d branch(es) failed to push", pushFailed))
	} else {
		s.recordSyncOK(key)
	}
}

// remoteAllowsPush 判断远端是否授权推送某分支。
func remoteAllowsPush(rm config.Remote, branch string) bool {
	for _, b := range rm.Push {
		if b == branch || b == "*" {
			return true
		}
	}
	return false
}

// pushOneBranch 推送单条分支（复用 execPush 的传输层逻辑，但不走 DSL 解析）。
func (s *Server) pushOneBranch(c Caller, project string, rm config.Remote, repo *gitstore.Repo, branch string) (string, error) {
	// 直接构造 "push <project> <remote> <branch>" 走同一条代码路径，
	// 避免两套推送逻辑各自演化。
	res, err := s.execPush(project+" "+rm.Name+" "+branch, c)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func (s *Server) recordSyncOK(key string) {
	s.syncState.mu.Lock()
	defer s.syncState.mu.Unlock()
	if s.syncState.lastSync == nil {
		s.syncState.lastSync = map[string]time.Time{}
	}
	if s.syncState.lastErr == nil {
		s.syncState.lastErr = map[string]string{}
	}
	s.syncState.lastSync[key] = time.Now()
	delete(s.syncState.lastErr, key)
}

func (s *Server) recordSyncErr(key, msg string) {
	s.syncState.mu.Lock()
	defer s.syncState.mu.Unlock()
	if s.syncState.lastErr == nil {
		s.syncState.lastErr = map[string]string{}
	}
	s.syncState.lastErr[key] = msg
}

func (s *Server) addSyncConflict(c syncConflict) {
	s.syncState.mu.Lock()
	defer s.syncState.mu.Unlock()
	if s.syncState.conflicts == nil {
		s.syncState.conflicts = map[string]syncConflict{}
	}
	s.syncState.conflicts[c.Project+"@"+c.Branch] = c
}

// SyncConflicts 返回待决议冲突的副本（供状态输出与 WebUI）。
func (s *Server) SyncConflicts() []syncConflict {
	s.syncState.mu.Lock()
	defer s.syncState.mu.Unlock()
	out := make([]syncConflict, 0, len(s.syncState.conflicts))
	for _, c := range s.syncState.conflicts {
		out = append(out, c)
	}
	return out
}

// ClearSyncConflict 人工处理完冲突后清掉记录。
func (s *Server) ClearSyncConflict(project, branch string) {
	s.syncState.mu.Lock()
	defer s.syncState.mu.Unlock()
	delete(s.syncState.conflicts, project+"@"+branch)
}

// syncTargets 列出所有纳入自动同步的 (project, remote)。
func (s *Server) syncTargets() []struct {
	Project string
	Remote  config.Remote
} {
	s.cfgMu.RLock()
	cf := s.cfgFile
	s.cfgMu.RUnlock()
	if cf == nil {
		return nil
	}
	var out []struct {
		Project string
		Remote  config.Remote
	}
	for _, rm := range cf.Remotes {
		if rm.AutoSync {
			out = append(out, struct {
				Project string
				Remote  config.Remote
			}{rm.Project, rm})
		}
	}
	return out
}

// startSyncLoop 启动后台同步循环。
//
// 触发条件（用户定的策略）：2 小时 或 数据量限制，看哪个先到。
// 前一半由 maintain.go 的 [commit] 判定完成，它折叠成功后调 kickSync；
// 后一半由这里的 ticker 兜底——覆盖「本地没写任何东西但远端有新提交」
// 这种只读场景，否则一台只读机器永远拉不到别人的更新。
func (s *Server) startSyncLoop() {
	s.cfgMu.RLock()
	cf := s.cfgFile
	s.cfgMu.RUnlock()

	interval := 2 * time.Hour
	if cf != nil && cf.Sync.Interval > 0 {
		interval = cf.Sync.Interval
	}

	st := &s.syncState
	st.mu.Lock()
	if st.stop != nil {
		st.mu.Unlock()
		return // 已在跑
	}
	stop := make(chan struct{})
	kick := make(chan struct{}, 1)
	st.stop, st.kick = stop, kick
	st.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// 启动后先等 30 秒再跑第一轮：给索引与配置加载留出时间，
		// 也避免服务刚起来就把网络打满。
		first := time.After(30 * time.Second)
		for {
			select {
			case <-stop:
				return
			case <-s.done:
				return
			case <-first:
				s.runSyncPass("startup")
			case <-ticker.C:
				s.runSyncPass("periodic")
			case <-kick:
				// 提交触发：稍等合并连续提交，但别等太久——
				// 用户刚写下的记忆应该几秒内就开始上传，而不是几十秒后。
				select {
				case <-time.After(2 * time.Second):
				case <-stop:
					return
				}
				s.runSyncPass("commit")
			}
		}
	}()
}

// kickSync 请求一次尽快同步（提交成功后调用）。非阻塞、去重。
func (s *Server) kickSync() {
	st := &s.syncState
	st.mu.Lock()
	kick := st.kick
	st.mu.Unlock()
	if kick == nil {
		return
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

// runSyncPass 对所有 auto_sync 目标跑一轮。
func (s *Server) runSyncPass(reason string) {
	targets := s.syncTargets()
	if len(targets) == 0 {
		return
	}
	for _, t := range targets {
		s.syncOnce(t.Project, t.Remote)
	}
	_ = reason
}

// syncStatusLines 给 status 动词报告同步现场。
//
// 只对全命名空间的调用者返回：远端列表和冲突路径都是跨项目信息，
// 受限凭证看到它们就等于能数出别的项目在跟谁同步、改了什么。
func (s *Server) syncStatusLines(c Caller) []string {
	if !c.Mount.Empty() {
		return nil
	}
	targets := s.syncTargets()
	s.syncState.mu.Lock()
	defer s.syncState.mu.Unlock()

	var out []string
	for _, t := range targets {
		key := t.Project + "@" + t.Remote.Name
		line := fmt.Sprintf("%s -> %s", t.Project, t.Remote.Name)
		if at, ok := s.syncState.lastSync[key]; ok {
			line += fmt.Sprintf(" (last %s ago)", time.Since(at).Truncate(time.Second))
		} else {
			line += " (never)"
		}
		if e := s.syncState.lastErr[key]; e != "" {
			line += " [" + e + "]"
		}
		out = append(out, line)
	}
	for _, cf := range s.syncState.conflicts {
		out = append(out, fmt.Sprintf("CONFLICT %s@%s: %s",
			cf.Project, cf.Branch, strings.Join(cf.Paths, ", ")))
	}
	return out
}
