package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitremote"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/gittransport"
)

// 记忆仓库的远端操作。
//
// 四条铁律：
//  1. 不自动 push。写入只落本地；推送必须是一次显式调用。
//  2. push 不隐式提交。脏层里的待提交内容如实报告，不替调用者做决定。
//  3. 不强推。覆盖远端历史等于抹掉别人已经写下的东西。
//  4. 冲突不静默。非快进时保留两边并报错，等显式决议。

// remotesOf 返回某个项目当前生效的远端声明。
func (s *Server) remotesOf(project string) []config.Remote {
	p := s.remotes.Load()
	if p == nil {
		return nil
	}
	var out []config.Remote
	for _, r := range *p {
		if r.Project == project {
			out = append(out, r)
		}
	}
	return out
}

// findRemote 按名字找远端。
func (s *Server) findRemote(project, name string) (config.Remote, error) {
	all := s.remotesOf(project)
	if len(all) == 0 {
		return config.Remote{}, fmt.Errorf("project %s has no declared remotes (add [[remote]] to the config)", project)
	}
	if name == "" {
		if len(all) == 1 {
			return all[0], nil
		}
		names := make([]string, 0, len(all))
		for _, r := range all {
			names = append(names, r.Name)
		}
		sort.Strings(names)
		return config.Remote{}, fmt.Errorf("project %s has multiple remotes; specify one: %s", project, strings.Join(names, " / "))
	}
	for _, r := range all {
		if r.Name == name {
			return r, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, r := range all {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return config.Remote{}, fmt.Errorf("project %s has no remote %q (available: %s)", project, name, strings.Join(names, ", "))
}

// execRemote 处理 remote 动词：列出某个项目的远端声明。
func (s *Server) execRemote(rest string, c Caller) (TermResult, error) {
	project := strings.TrimSpace(rest)
	all := s.remotes.Load()
	if all == nil {
		return TermResult{Text: "no remotes configured"}, nil
	}
	rules := *all
	if project == "" {
		var sb strings.Builder
		byProject := map[string][]config.Remote{}
		for _, r := range rules {
			byProject[r.Project] = append(byProject[r.Project], r)
		}
		if len(byProject) == 0 {
			return TermResult{Text: "no remotes configured"}, nil
		}
		var projects []string
		for p := range byProject {
			projects = append(projects, p)
		}
		sort.Strings(projects)
		for _, p := range projects {
			if !s.projectVisibleFor(c, p) {
				continue
			}
			for _, r := range byProject[p] {
				sb.WriteString(renderRemote(p, r) + "\n")
			}
		}
		return TermResult{
			Text:       strings.TrimRight(sb.String(), "\n"),
			Structured: map[string]any{"remotes": rules},
		}, nil
	}

	if !s.projectVisibleFor(c, project) {
		return TermResult{}, fmt.Errorf("project %q is outside this credential's visible scope", project)
	}
	var sb strings.Builder
	n := 0
	for _, r := range rules {
		if r.Project != project {
			continue
		}
		sb.WriteString(renderRemote(project, r) + "\n")
		n++
	}
	if n == 0 {
		return TermResult{}, fmt.Errorf("project %s has no declared remotes", project)
	}
	return TermResult{Text: strings.TrimRight(sb.String(), "\n"), Structured: map[string]any{"remotes": rules}}, nil
}

// toGitRemote 把配置里的远端声明转成执行层结构。
func toGitRemote(r config.Remote) gitremote.Remote {
	return gitremote.Remote{
		Name:         r.Name,
		URL:          r.URL,
		FetchOnly:    r.FetchOnly,
		PushBranches: r.Push,
	}
}

func renderRemote(project string, r config.Remote) string {
	mode := "read/write"
	if r.FetchOnly {
		mode = "read-only"
	}
	push := "push disabled"
	if len(r.Push) > 0 {
		push = "push allowed " + strings.Join(r.Push, ",")
	}
	return fmt.Sprintf("%-20s %-10s %-28s %s %s", project, r.Name, gitremote.Redact(r.URL), mode, push)
}

// remoteStallTimeout 是内置传输的静默窗口：允许对端安静多久。
//
// 它**不是**单次操作的总预算（2026-11-24 改）。旧值 90 秒当总预算用时，
// 3060 冷页缓存下算一次「结果为空」的全量闭包就把它耗尽，报出来的是一行
// 「读响应超时」——看起来像网络故障，实际是判据选错了：对端一直在活着，
// 只是算得慢。语义与默认值见 gittransport 的 idle.go。
const remoteStallTimeout = gittransport.DefaultStallTimeout

// execFetch 处理 fetch <project> [remote]：把远端历史取回本地。
//
// 只更新 refs/remotes/<remote>/*，不动本地分支。
// 「知道远端有什么」和「把远端变成我的」是两件事。
func (s *Server) execRemoteFetch(rest string, c Caller) (TermResult, error) {
	project, name := splitProjectRemote(rest)
	if project == "" {
		return TermResult{}, fmt.Errorf("fetch requires <project> [<remote>]")
	}
	if !s.projectVisibleFor(c, project) {
		return TermResult{}, fmt.Errorf("project %q is outside this credential's visible scope", project)
	}
	rm, err := s.findRemote(project, name)
	if err != nil {
		return TermResult{}, err
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		return TermResult{}, err
	}

	res, err := s.fetchRemote(c, project, rm, repo)
	if err != nil {
		return TermResult{}, fmt.Errorf("fetch %s@%s failed: %w", project, rm.Name, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "fetched %s <- %s (built-in transport)\n", project, rm.Name)
	if len(res.Branches) == 0 {
		sb.WriteString("(no readable branches)")
	}
	names := make([]string, 0, len(res.Branches))
	for b := range res.Branches {
		names = append(names, b)
	}
	sort.Strings(names)
	for _, b := range names {
		fmt.Fprintf(&sb, "  %-16s %s\n", b, shortSHA(res.Branches[b]))
	}
	return TermResult{
		Text: strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{
			"project": project, "remote": rm.Name, "branches": res.Branches, "objects": res.Objects,
		},
	}, nil
}

// fetchBranches 回答「这个远端允许取回哪些分支」。
//
// 取回哪些分支必须是显式配置的：[[remote]] 的 pull 优先，为空则回落到 push
// （「只推不拉」由 push_only 单独表达，不靠列表为空来暗示）。两边都没写
// 就是这个远端没有任何取回意图，此时去取任何东西都是猜的。
//
// 为什么不让「空」等于「全取」（2026-11-24 3060 链路）：对端广告什么就取
// 什么，等于把「同步范围」交给对端单方面决定。weft 的 meta.index 是每台
// 机器各自重建的索引快照（98 个 blob / 246 MB），一旦被 advertisement 带进
// want，一次 fetch 就在打包上耗尽 90 秒预算，表现为「读响应超时」。
// "*" 是唯一的通配，用来显式表达「这个远端就是要全取」。
func fetchBranches(pull, push []string, pushOnly bool) []string {
	if pushOnly {
		return nil
	}
	if len(pull) > 0 {
		return pull
	}
	return push
}

// branchAllowed 回答「这串白名单里是否允许某个分支」，"*" 是通配。
func branchAllowed(scope []string, branch string) bool {
	for _, s := range scope {
		if s == "*" || s == branch {
			return true
		}
	}
	return false
}

// pruneRemoteRefs 把 refs/remotes/<remote>/* 对齐到配置声明的取回范围。
//
// 为什么必须剪（2026-11-24 3060 链路）：取回范围缩小之后，上一次按旧范围
// 取回来的跟踪引用还留着；而 mergeRemoteRefs 只看 refs/remotes/<rm>/，
// 于是那些「已经不在配置里」的分支每轮仍然被合并进本地分支，
// 冲突也永远解不掉（现场表现：meta.index 一条 bge-m3.json 冲突挂在那里）。
// 配置说不要，就必须真的不要——否则「显式声明范围」只是一句闲话。
//
// 返回被剪掉的分支名，供调用方出声。
func pruneRemoteRefs(repo *gitstore.Repo, remote string, scope []string) ([]string, error) {
	prefix := "refs/remotes/" + remote + "/"
	refs, err := repo.Refs(prefix)
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, ref := range refs {
		branch := strings.TrimPrefix(ref, prefix)
		if branch == "" || branch == ref {
			continue
		}
		if branchAllowed(scope, branch) {
			continue
		}
		if err := repo.DeleteRemoteRef(remote, branch); err != nil {
			return dropped, err
		}
		dropped = append(dropped, branch)
	}
	return dropped, nil
}

// fetchRemote 按远端类型分派取回。凭证只从服务端密钥库读。
func (s *Server) fetchRemote(c Caller, project string, rm config.Remote, repo *gitstore.Repo) (gittransport.FetchResult, error) {
	ctx := context.Background()
	branches := fetchBranches(rm.Pull, rm.Push, rm.PushOnly)
	if len(branches) == 0 {
		if rm.PushOnly {
			return gittransport.FetchResult{}, fmt.Errorf("remote %q is push_only: it never fetches anything back", rm.Name)
		}
		return gittransport.FetchResult{}, fmt.Errorf("remote %q declares no pull branches: add pull = [\"...\"] (or push, for a remote that pushes and pulls the same branches) to [[remote]]", rm.Name)
	}
	switch {
	case gitremote.IsSSH(rm.URL):
		key, err := s.readSSHKey(rm.SSHKey)
		if err != nil {
			return gittransport.FetchResult{}, fmt.Errorf("read SSH private key %q: %w", rm.SSHKey, err)
		}
		res, err := gittransport.FetchSSH(ctx, repo, rm.URL, rm.Name, string(key), rm.HostKey, branches, remoteStallTimeout)
		if err != nil {
			return res, err
		}
		return res, s.keepReadableBranches(c, project, repo, rm.Name, branches, res)
	case strings.HasPrefix(rm.URL, "http://"), strings.HasPrefix(rm.URL, "https://"):
		token, err := s.optionalSecret(rm.HTTPToken, "http_token")
		if err != nil {
			return gittransport.FetchResult{}, err
		}
		res, err := gittransport.FetchHTTP(ctx, repo, rm.URL, rm.Name, rm.HTTPUser, token, branches, remoteStallTimeout)
		if err != nil {
			return res, err
		}
		return res, s.keepReadableBranches(c, project, repo, rm.Name, branches, res)
	}
	if _, ok := gittransport.LocalPath(rm.URL); ok {
		res, err := gittransport.FetchLocal(repo, rm.URL, rm.Name, branches)
		if err != nil {
			return gittransport.FetchResult{}, err
		}
		return res, s.keepReadableBranches(c, project, repo, rm.Name, branches, res)
	}
	return gittransport.FetchResult{}, fmt.Errorf("unsupported remote transport: %s", gitremote.Redact(rm.URL))
}

// keepReadableBranches 把结果裁到调用者能读的范围，并删掉不该留的跟踪引用。
//
// 取回发生在服务端：一次 fetch 会把远端**所有**分支的对象写进本地对象库。
// 对象层面无法按分支隔离（内容寻址），所以边界只能画在「返回什么」上，
// 并且不写回调用者读不到的分支指针。
//
// 同时把跟踪引用剪到配置声明的取回范围（scope）之内：两者都是「哪些
// 分支不算数」的判据，分开写一定会漂移。
func (s *Server) keepReadableBranches(c Caller, project string, repo *gitstore.Repo, remote string, scope []string, res gittransport.FetchResult) error {
	dropped, err := pruneRemoteRefs(repo, remote, scope)
	if err != nil {
		return err
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		s.logf("fetch %s <- %s: dropped %d tracking ref(s) outside the configured scope: %s",
			project, remote, len(dropped), strings.Join(dropped, ", "))
	}
	for branch := range res.Branches {
		if s.allowed(c, project, branch, configBitRead) {
			continue
		}
		delete(res.Branches, branch)
		if err := repo.DeleteRemoteRef(remote, branch); err != nil {
			return err
		}
	}
	return nil
}

// execPush 处理 push <project> [remote] <branch...>。
func (s *Server) execPush(rest string, c Caller) (TermResult, error) {
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return TermResult{}, fmt.Errorf("push requires <project> [<remote>] <branch...>\n" +
			"  specifying no branch is rejected: pushing all by default could publish everything by mistake")
	}
	// 快照导出开关。--snapshot 把当前树打成一个孤儿提交再推；
	// --force 才允许改写远端已有的历史（快照永远不是快进）。
	snapshot, force := false, false
	{
		kept := make([]string, 0, len(fields))
		for _, f := range fields {
			switch f {
			case "--snapshot":
				snapshot = true
			case "--force", "-f", "--force-snapshot":
				force = true
			default:
				kept = append(kept, f)
			}
		}
		fields = kept
	}
	if len(fields) < 2 {
		return TermResult{}, fmt.Errorf("push requires <project> [<remote>] <branch...>\n" +
			"  specifying no branch is rejected: pushing all by default could publish everything by mistake")
	}

	project := fields[0]
	rmName := ""
	branches := fields[1:]
	if len(fields) >= 3 {
		// 第二段若不是分支名（含 / 或与已声明远端同名），当作远端名。
		if _, err := s.findRemote(project, fields[1]); err == nil {
			rmName = fields[1]
			branches = fields[2:]
		}
	}
	if len(branches) == 0 {
		return TermResult{}, fmt.Errorf("push requires an explicitly specified branch")
	}
	if len(branches) != 1 {
		return TermResult{}, errors.New("the built-in SSH transport pushes one branch at a time; confirm multiple branches individually to avoid mistaking partial success for complete success")
	}
	// 逐分支判写权限：能推主干不等于能推别的分支。
	// 推送是「让远端历史前进」，它必须和本地写入走同一把尺子。
	for _, br := range branches {
		branch := strings.TrimPrefix(br, "refs/heads/")
		if i := strings.Index(branch, ":"); i >= 0 {
			branch = branch[:i]
		}
		if !s.allowed(c, project, branch, configBitWrite) {
			return TermResult{}, fmt.Errorf("%s", s.denyReason(c, project, branch, configBitWrite))
		}
	}

	rm, err := s.findRemote(project, rmName)
	if err != nil {
		return TermResult{}, err
	}
	// 不隐式提交：有未提交内容就明确告诉调用方，由它决定是否先 commit。
	//
	// 只数**这条分支**：别的分支脏不该拦住这一条（见 pendingOnBranch 的说明）。
	for _, br := range branches {
		branch := strings.TrimPrefix(br, "refs/heads/")
		if i := strings.Index(branch, ":"); i >= 0 {
			branch = branch[:i]
		}
		if n := s.pendingOnBranch(project, branch); n > 0 {
			return TermResult{}, fmt.Errorf(
				"%s@%s has %d uncommitted change(s): push does not commit for you. Commit first, then push",
				project, branch, n)
		}
	}

	if rm.FetchOnly {
		return TermResult{}, fmt.Errorf("remote %s is read-only (fetch_only)", rm.Name)
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		return TermResult{}, err
	}
	fetchOnly := toGitRemote(rm)
	branch := strings.TrimPrefix(branches[0], "refs/heads/")
	if strings.Contains(branch, ":") {
		return TermResult{}, errors.New("push accepts branch names only, not refspecs")
	}
	if !fetchOnly.AllowsPush(branch) {
		return TermResult{}, fmt.Errorf("remote %s does not authorize pushing branch %q", rm.Name, branch)
	}

	// 快照导出：把当前树打成一个没有父提交的孤儿提交。
	//
	// 它不改本地任何引用——本地历史原样保留，只有推出去的那一份是
	// 单提交快照。这正是「只浅拷贝去其他仓库，历史都不要」。
	pushOpts := gittransport.PushOptions{AllowNonFastForward: force}
	snapshotSHA := ""
	if snapshot {
		orphan, _, oerr := repo.CreateOrphanCommit(branch, "")
		if oerr != nil {
			return TermResult{}, fmt.Errorf("create snapshot commit for %s@%s: %w", project, branch, oerr)
		}
		snapshotSHA = orphan
		pushOpts.SourceSHA = orphan
	}

	var res gittransport.PushResult
	switch {
	case gitremote.IsSSH(rm.URL):
		if rm.SSHKey == "" || rm.HostKey == "" {
			return TermResult{}, errors.New("SSH push requires ssh_key and host_key (SHA256 fingerprint) in [[remote]]")
		}
		key, err := s.readSSHKey(rm.SSHKey)
		if err != nil {
			return TermResult{}, fmt.Errorf("read SSH private key %q: %w", rm.SSHKey, err)
		}
		res, err = gittransport.Push(context.Background(), repo, rm.URL, branch, string(key), rm.HostKey, remoteStallTimeout, pushOpts)
		if err != nil {
			return TermResult{}, fmt.Errorf("push %s@%s -> %s failed: %w", project, branch, rm.Name, err)
		}
	case strings.HasPrefix(rm.URL, "http://"), strings.HasPrefix(rm.URL, "https://"):
		if rm.HTTPToken == "" {
			return TermResult{}, errors.New("HTTP push requires http_token in [[remote]] (the token name stored with remote-secret)")
		}
		token, err := s.readSecret(rm.HTTPToken)
		if err != nil {
			return TermResult{}, fmt.Errorf("read token %q: %w", rm.HTTPToken, err)
		}
		res, err = gittransport.PushHTTP(context.Background(), repo, rm.URL, branch, rm.HTTPUser, token, remoteStallTimeout, pushOpts)
		if err != nil {
			return TermResult{}, fmt.Errorf("push %s@%s -> %s failed: %w", project, branch, rm.Name, err)
		}
	default:
		if _, ok := gittransport.LocalPath(rm.URL); !ok {
			return TermResult{}, fmt.Errorf("unsupported remote transport: %s (supported: ssh:// user@host:path, http(s)://, and local bare repository paths)", gitremote.Redact(rm.URL))
		}
		res, err = gittransport.PushLocal(repo, rm.URL, branch, pushOpts)
		if err != nil {
			return TermResult{}, fmt.Errorf("push %s@%s -> %s failed: %w", project, branch, rm.Name, err)
		}
	}

	var sb strings.Builder
	if snapshotSHA != "" {
		fmt.Fprintf(&sb, "pushed %s@%s -> %s as a SNAPSHOT (single orphan commit, no history)\n", project, branch, rm.Name)
		fmt.Fprintf(&sb, "  snapshot commit %s (local history untouched)\n", shortSHA(snapshotSHA))
	} else {
		fmt.Fprintf(&sb, "pushed %s -> %s (built-in transport; does not invoke system git)\n", project, rm.Name)
	}
	fmt.Fprintf(&sb, "  %s %s -> %s\n", res.Branch, shortSHA(res.Old), shortSHA(res.New))
	return TermResult{
		Text: strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{
			"project": project, "remote": rm.Name, "refs": []gittransport.PushResult{res},
		},
	}, nil
}

// execWorkPeerSync 对手动 `sync` 做一次真实的工作层双向同步。
//
// 与后台循环走的同步函数完全一致，不另开一条实现：
// 两条路径一旦分叉，就一定会出现「手动能拉、后台不能」这种诡异现象。
func (s *Server) execWorkPeerSync(project string, rm config.Remote, repo *gitstore.Repo, c Caller) (TermResult, error) {
	if err := s.syncWorkWithRemote(project, rm, repo); err != nil {
		return TermResult{}, fmt.Errorf("sync %s@%s failed: %w", project, rm.Name, err)
	}

	// 如实报出每条分支的水位：out 追上去了就说明推到了，
	// in 追上去了就说明拉到了。两个都是 0 就是「没事可做」。
	branches := make([]string, 0, len(rm.Push)+len(rm.Pull))
	seen := map[string]bool{}
	for _, br := range append(append([]string{}, rm.Push...), rm.Pull...) {
		if br == "*" || seen[br] {
			continue
		}
		seen[br] = true
		branches = append(branches, br)
	}
	sort.Strings(branches)

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s <-> %s (work-layer peer %s)\n", project, rm.Name, gitremote.Redact(rm.URL))
	rows := make([]map[string]any, 0, len(branches))
	for _, br := range branches {
		if !s.allowed(c, project, br, configBitRead) {
			continue
		}
		o, i := s.workSync.cursors(project, br, rm.Name)
		// 这三行说的是**方向**与**水位**，不是「本轮推了几个字节」。
		// 曾经零传输的那一轮不记水位，于是「同步过、两边一致」显示成
		// pushed、「从没同步过」显示成 no work-layer change——两条分支
		// 内容完全相同时一个 pushed 一个 no change，看着像一条在传一条
		// 没传。现在零传输也记水位，两种状态不再混在一起。
		state := "in sync"
		switch {
		case i == 0 && o == 0:
			state = "never synced"
		case o > 0 && i == 0:
			state = "pushed out"
		case i > 0 && o == 0:
			state = "pulled in"
		}
		fmt.Fprintf(&sb, "  %-20s %s (out seq %d, in seq %d)\n", br, state, o, i)
		rows = append(rows, map[string]any{"branch": br, "state": state, "out_seq": o, "in_seq": i})
	}
	if len(branches) == 0 {
		sb.WriteString("  (no branches declared in push/pull; nothing to sync)\n")
	}
	return TermResult{
		Text: strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{
			"project": project, "remote": rm.Name, "branches": rows,
		},
	}, nil
}

// execSync 处理 sync <project> [remote]：报告本地与远端的位置关系。
//
// 只读广告，不下载对象：比对「谁领先谁落后」不需要内容。
// 分支是否可读、远端跟踪引用是否存在，都在这层判定。
func (s *Server) execSync(rest string, c Caller) (TermResult, error) {
	project, name := splitProjectRemote(rest)
	if project == "" {
		// 无参的 sync = 同步流水视图。
		//
		// 这是 agent 问「记忆体什么情况」时该看到的：本机到哪了、
		// 对端到哪了、最近几轮推拉的结果。写进 stderr 日志等于只有
		// 翻机器的人看得见；而上下游两边的 agent 都需要自己能读到。
		return s.syncVerbNoArgs(c)
	}
	if !s.projectVisibleFor(c, project) {
		return TermResult{}, fmt.Errorf("project %q is outside this credential's visible scope", project)
	}
	rm, err := s.findRemote(project, name)
	if err != nil {
		return TermResult{}, err
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		return TermResult{}, err
	}

	// 工作层对端（URL 不以 .git 结尾）：不是 Git 仓库，没有 ref 广告可比。
	// 手动 sync 在这里的含义就是「现在就跑一次双向同步」，而不是报位置。
	//
	// 之前它直接落到下面的 listRemote（git 协议探测），对这种 URL
	// 只能报 unsupported remote transport——也就是说手动触发根本拉不动
	// 工作层，只能等后台循环。那让「我很急，现在就同步」变成不可能。
	if isWorkPeerURL(rm.URL) {
		return s.execWorkPeerSync(project, rm, repo, c)
	}

	remoteRefs, err := s.listRemote(rm)
	if err != nil {
		return TermResult{}, fmt.Errorf("read remote advertisement from %s: %w", rm.Name, err)
	}

	// 取回范围：本报告要能区分「本该拉却没拉」与「刻意不同步」。
	scope := fetchBranches(rm.Pull, rm.Push, rm.PushOnly)

	localRefs, _ := repo.Refs("refs/heads/")
	seen := map[string]bool{}
	var branches []string
	for _, ref := range localRefs {
		b := strings.TrimPrefix(ref, "refs/heads/")
		if seen[b] || !s.allowed(c, project, b, configBitRead) {
			continue
		}
		seen[b] = true
		branches = append(branches, b)
	}
	for ref := range remoteRefs {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		b := strings.TrimPrefix(ref, "refs/heads/")
		if seen[b] || !s.allowed(c, project, b, configBitRead) {
			continue
		}
		seen[b] = true
		branches = append(branches, b)
	}
	sort.Strings(branches)

	type row struct {
		Branch string `json:"branch"`
		Local  string `json:"local,omitempty"`
		Remote string `json:"remote,omitempty"`
		State  string `json:"state"`
	}
	rows := make([]row, 0, len(branches))
	var sb strings.Builder
	pending := s.pendingCount(project)
	fmt.Fprintf(&sb, "%s <-> %s (%s)", project, rm.Name, gitremote.Redact(rm.URL))
	if pending > 0 {
		fmt.Fprintf(&sb, "  %d uncommitted changes locally", pending)
	}
	sb.WriteString("\n")

	for _, b := range branches {
		local, _ := repo.Resolve(b)
		remote := remoteRefs["refs/heads/"+b]
		state := classifySync(repo, local, remote)
		// 不在取回范围里的分支要单独标，不能叫 not-fetched。
		//
		// 为什么（2026-11-24）：meta.index 是每台机器各自重建的索引快照，
		// 配置里从没声明要取它。报成 not-fetched 会让人以为「本该拉却没拉」，
		// 于是去查一个根本不存在的故障；而它真正的关系是「刻意不同步」。
		if state != "synced" && !branchAllowed(scope, b) {
			state = "out-of-scope"
		}
		rows = append(rows, row{Branch: b, Local: local, Remote: remote, State: state})
		fmt.Fprintf(&sb, "  %-16s %-12s local %s remote %s\n", b, state, shortSHA(local), shortSHA(remote))
	}
	if len(rows) == 0 {
		sb.WriteString("(no readable branches)")
	}

	// 自动同步的现场：上次同步时间、待决议冲突、上次错误。
	// 排查「为什么我的记忆没到另一台机器」时，这三个比分支状态更有用。
	if rm.AutoSync {
		key := project + "@" + rm.Name
		if v, ok := s.syncState.lastSync.Load(key); ok {
			if at, ok := v.(time.Time); ok {
				fmt.Fprintf(&sb, "  auto-sync: last %s ago\n", time.Since(at).Truncate(time.Second))
			}
		} else {
			sb.WriteString("  auto-sync: never\n")
		}
		if v, ok := s.syncState.lastErr.Load(key); ok {
			if e, _ := v.(string); e != "" {
				fmt.Fprintf(&sb, "  auto-sync last error: %s\n", e)
			}
		}
		var myConflicts []syncConflict
		s.syncState.conflicts.Range(func(_, cv any) bool {
			if c, ok := cv.(syncConflict); ok && c.Project == project {
				myConflicts = append(myConflicts, c)
			}
			return true
		})
		if len(myConflicts) > 0 {
			sb.WriteString("  CONFLICTS awaiting manual resolution:\n")
			for _, c := range myConflicts {
				fmt.Fprintf(&sb, "    %s@%s (%d path): %s\n",
					c.Project, c.Branch, len(c.Paths), strings.Join(c.Paths, ", "))
			}
		}
	}

	return TermResult{
		Text: strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{
			"project": project, "remote": rm.Name, "pending": pending, "branches": rows,
		},
	}, nil
}

// classifySync 判断一条分支相对远端的位置。
//
// 只凭 advertisement 只能知道 sha 是否相等；要分「领先」「落后」「分叉」
// 必须能沿提交图走，也就是远端历史已经被 fetch 过。走不了就诚实说 unknown，
// 不猜——把分叉报成落后，会让人做出错误的重置决定。
func classifySync(repo *gitstore.Repo, local, remote string) string {
	switch {
	case local == "" && remote == "":
		return "unknown"
	case local == "":
		return "remote-only"
	case remote == "":
		return "local-only"
	case local == remote:
		return "synced"
	}
	localHasRemote, err1 := repo.IsAncestor(remote, local)
	remoteHasLocal, err2 := repo.IsAncestor(local, remote)
	switch {
	case err1 != nil || err2 != nil:
		return "not-fetched"
	case localHasRemote && !remoteHasLocal:
		return "ahead"
	case remoteHasLocal && !localHasRemote:
		return "behind"
	default:
		return "diverged"
	}
}

// listRemote 只取远端广告。sync 用它，避免为了比对而下载整段历史。
func (s *Server) listRemote(rm config.Remote) (map[string]string, error) {
	ctx := context.Background()
	switch {
	case gitremote.IsSSH(rm.URL):
		key, err := s.readSSHKey(rm.SSHKey)
		if err != nil {
			return nil, fmt.Errorf("read SSH private key %q: %w", rm.SSHKey, err)
		}
		return gittransport.ListSSH(ctx, rm.URL, string(key), rm.HostKey, remoteStallTimeout)
	case strings.HasPrefix(rm.URL, "http://"), strings.HasPrefix(rm.URL, "https://"):
		token, err := s.optionalSecret(rm.HTTPToken, "http_token")
		if err != nil {
			return nil, err
		}
		return gittransport.ListHTTP(ctx, rm.URL, rm.HTTPUser, token, remoteStallTimeout)
	}
	if _, ok := gittransport.LocalPath(rm.URL); ok {
		return gittransport.ListLocal(rm.URL)
	}
	return nil, fmt.Errorf("unsupported remote transport: %s", gitremote.Redact(rm.URL))
}

// splitProjectRemote 解析 "<project> [<remote>]"。
func splitProjectRemote(rest string) (project, remote string) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", ""
	}
	project = fields[0]
	if len(fields) > 1 {
		remote = fields[1]
	}
	return project, remote
}

// pendingCount 返回某个项目所有分支上的未提交条目数。
//
// 它用来回答「现在 push 会不会漏东西」：push 只搬已存在的提交，
// 而脏层里那些未封箱的写入还不在任何提交里。把这件事说清楚，
// 比「自动帮你提交一下」安全得多——后者会替调用者做语义决定。
func (s *Server) pendingCount(project string) int {
	repo, err := s.projects.Get(project)
	if err != nil {
		return 0
	}
	n := 0
	for _, ref := range repo.PendingRefs() {
		n += repo.Pending(ref) + len(repo.Deleted(ref))
	}
	return n
}

// pendingOnBranch 只统计**某一条分支**上未折叠的改动。
//
// 为什么推送要按分支数（2026-11-25 实测）：
//
//	pendingCount 数的是整个项目所有分支的脏改动，而推送只推一条分支。
//	本机 meta.index（向量索引快照链，246MB，不在任何 remote 的推送范围里）
//	长期挂着未折叠的改动，于是 main.chat 的每一次推送都被它拦下来：
//
//	  push main.chat failed: project BlackEggAI/zero has 1 uncommitted changes
//
//	那条脏分支根本没打算推，也没人打算提交它——结果是一条完全无关的
//	分支把真正的同步永久卡住，日志里只有一句「1 个未提交改动」。
//
// 判据必须是「你要推的这条分支有没有未提交内容」，不是「这个项目脏不脏」。
func (s *Server) pendingOnBranch(project, branch string) int {
	repo, err := s.projects.Get(project)
	if err != nil {
		return 0
	}
	return repo.Pending(branch) + len(repo.Deleted(branch))
}

// projectVisibleFor 报告凭证能否看到该项目（任意分支可读即可）。
//
// 与 HTTP Git 端点用同一条判据：两个入口必须对「谁能看到这个项目」
// 给出同一个答案，否则受限凭证可以从弱的那一侧绕过去。
func (s *Server) projectVisibleFor(c Caller, project string) bool {
	repo, err := s.projects.Get(project)
	if err != nil {
		branch := s.defaultBranch
		return s.allowed(c, project, branch, configBitRead)
	}
	refs, err := repo.Refs("refs/heads/")
	if err != nil {
		return false
	}
	if len(refs) == 0 {
		branch := s.defaultBranch
		return s.allowed(c, project, branch, configBitRead)
	}
	for _, ref := range refs {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if s.allowed(c, project, branch, configBitRead) {
			return true
		}
	}
	return false
}
