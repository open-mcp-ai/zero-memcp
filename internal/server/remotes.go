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
		return config.Remote{}, fmt.Errorf("项目 %s 没有声明任何远端（在配置里加 [[remote]]）", project)
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
		return config.Remote{}, fmt.Errorf("项目 %s 有多个远端，请指定其一：%s", project, strings.Join(names, " / "))
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
	return config.Remote{}, fmt.Errorf("项目 %s 没有远端 %q（可用：%s）", project, name, strings.Join(names, ", "))
}

// execRemote 处理 remote 动词：列出某个项目的远端声明。
func (s *Server) execRemote(rest string, c Caller) (TermResult, error) {
	project := strings.TrimSpace(rest)
	all := s.remotes.Load()
	if all == nil {
		return TermResult{Text: "没有配置任何远端"}, nil
	}
	rules := *all
	if project == "" {
		var sb strings.Builder
		byProject := map[string][]config.Remote{}
		for _, r := range rules {
			byProject[r.Project] = append(byProject[r.Project], r)
		}
		if len(byProject) == 0 {
			return TermResult{Text: "没有配置任何远端"}, nil
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
		return TermResult{}, fmt.Errorf("项目 %q 不在这张凭证的可见范围内", project)
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
		return TermResult{}, fmt.Errorf("项目 %s 没有声明远端", project)
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
	mode := "读写"
	if r.FetchOnly {
		mode = "只读"
	}
	push := "不推"
	if len(r.Push) > 0 {
		push = "可推 " + strings.Join(r.Push, ",")
	}
	return fmt.Sprintf("%-20s %-10s %-28s %s %s", project, r.Name, gitremote.Redact(r.URL), mode, push)
}

// remoteTimeout 是内置传输单次操作的上限。
const remoteTimeout = 90 * time.Second

// execFetch 处理 fetch <project> [remote]：把远端历史取回本地。
//
// 只更新 refs/remotes/<remote>/*，不动本地分支。
// 「知道远端有什么」和「把远端变成我的」是两件事。
func (s *Server) execRemoteFetch(rest string, c Caller) (TermResult, error) {
	project, name := splitProjectRemote(rest)
	if project == "" {
		return TermResult{}, fmt.Errorf("fetch 需要 <project> [<remote>]")
	}
	if !s.projectVisibleFor(c, project) {
		return TermResult{}, fmt.Errorf("项目 %q 不在这张凭证的可见范围内", project)
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
		return TermResult{}, fmt.Errorf("fetch %s@%s 失败: %w", project, rm.Name, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "fetch %s ← %s（内置传输）\n", project, rm.Name)
	if len(res.Branches) == 0 {
		sb.WriteString("（没有可读分支）")
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

// fetchRemote 按远端类型分派取回。凭证只从服务端密钥库读。
func (s *Server) fetchRemote(c Caller, project string, rm config.Remote, repo *gitstore.Repo) (gittransport.FetchResult, error) {
	ctx := context.Background()
	switch {
	case gitremote.IsSSH(rm.URL):
		key, err := s.readSSHKey(rm.SSHKey)
		if err != nil {
			return gittransport.FetchResult{}, fmt.Errorf("读取 SSH 私钥 %q 失败: %w", rm.SSHKey, err)
		}
		res, err := gittransport.FetchSSH(ctx, repo, rm.URL, rm.Name, string(key), rm.HostKey, remoteTimeout)
		if err != nil {
			return res, err
		}
		return res, s.keepReadableBranches(c, project, repo, rm.Name, res)
	case strings.HasPrefix(rm.URL, "http://"), strings.HasPrefix(rm.URL, "https://"):
		token, err := s.optionalSecret(rm.HTTPToken, "http_token")
		if err != nil {
			return gittransport.FetchResult{}, err
		}
		res, err := gittransport.FetchHTTP(ctx, repo, rm.URL, rm.Name, rm.HTTPUser, token, remoteTimeout)
		if err != nil {
			return res, err
		}
		return res, s.keepReadableBranches(c, project, repo, rm.Name, res)
	}
	if _, ok := gittransport.LocalPath(rm.URL); ok {
		res, err := gittransport.FetchLocal(repo, rm.URL, rm.Name)
		if err != nil {
			return gittransport.FetchResult{}, err
		}
		return res, s.keepReadableBranches(c, project, repo, rm.Name, res)
	}
	return gittransport.FetchResult{}, fmt.Errorf("不支持的远端传输：%s", gitremote.Redact(rm.URL))
}

// keepReadableBranches 把结果裁到调用者能读的范围，并删掉不该留的跟踪引用。
//
// 取回发生在服务端：一次 fetch 会把远端**所有**分支的对象写进本地对象库。
// 对象层面无法按分支隔离（内容寻址），所以边界只能画在「返回什么」上，
// 并且不写回调用者读不到的分支指针。
func (s *Server) keepReadableBranches(c Caller, project string, repo *gitstore.Repo, remote string, res gittransport.FetchResult) error {
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
		return TermResult{}, fmt.Errorf("push 需要 <project> [<remote>] <branch...>\n" +
			"  不指定分支会被拒绝：默认推全部等于一次手滑就对外发布")
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
		return TermResult{}, fmt.Errorf("push 必须显式指定分支")
	}
	if len(branches) != 1 {
		return TermResult{}, errors.New("SSH 最小闭环一次只推一个分支；多分支逐个确认，避免部分成功被误认为整体成功")
	}
	// 逐分支判写权限：能推 main 不等于能推 me@chat。
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
	pending := s.pendingCount(project)
	if pending > 0 {
		return TermResult{}, fmt.Errorf(
			"项目 %s 有 %d 处未提交内容：push 不会替你 commit。先 commit 再 push", project, pending)
	}

	if rm.FetchOnly {
		return TermResult{}, fmt.Errorf("远端 %s 是只读的（fetch_only）", rm.Name)
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		return TermResult{}, err
	}
	fetchOnly := toGitRemote(rm)
	branch := strings.TrimPrefix(branches[0], "refs/heads/")
	if strings.Contains(branch, ":") {
		return TermResult{}, errors.New("push 只接受分支名，不接受 refspec")
	}
	if !fetchOnly.AllowsPush(branch) {
		return TermResult{}, fmt.Errorf("远端 %s 未授权推送分支 %q", rm.Name, branch)
	}

	var res gittransport.PushResult
	switch {
	case gitremote.IsSSH(rm.URL):
		if rm.SSHKey == "" || rm.HostKey == "" {
			return TermResult{}, errors.New("SSH push 需要在 [[remote]] 声明 ssh_key 与 host_key（SHA256 fingerprint）")
		}
		key, err := s.readSSHKey(rm.SSHKey)
		if err != nil {
			return TermResult{}, fmt.Errorf("读取 SSH 私钥 %q 失败: %w", rm.SSHKey, err)
		}
		res, err = gittransport.Push(context.Background(), repo, rm.URL, branch, string(key), rm.HostKey, 90*time.Second)
		if err != nil {
			return TermResult{}, fmt.Errorf("push %s@%s → %s 失败: %w", project, branch, rm.Name, err)
		}
	case strings.HasPrefix(rm.URL, "http://"), strings.HasPrefix(rm.URL, "https://"):
		if rm.HTTPToken == "" {
			return TermResult{}, errors.New("HTTP push 需要在 [[remote]] 声明 http_token（用 remote-secret 保存的 token 名）")
		}
		token, err := s.readSecret(rm.HTTPToken)
		if err != nil {
			return TermResult{}, fmt.Errorf("读取 token %q 失败: %w", rm.HTTPToken, err)
		}
		res, err = gittransport.PushHTTP(context.Background(), repo, rm.URL, branch, rm.HTTPUser, token, 90*time.Second)
		if err != nil {
			return TermResult{}, fmt.Errorf("push %s@%s → %s 失败: %w", project, branch, rm.Name, err)
		}
	default:
		if _, ok := gittransport.LocalPath(rm.URL); !ok {
			return TermResult{}, fmt.Errorf("不支持的远端传输：%s（只支持 ssh:// user@host:path、http(s):// 与本地裸仓库路径）", gitremote.Redact(rm.URL))
		}
		res, err = gittransport.PushLocal(repo, rm.URL, branch)
		if err != nil {
			return TermResult{}, fmt.Errorf("push %s@%s → %s 失败: %w", project, branch, rm.Name, err)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "push %s → %s（内置传输，不调用系统 git）\n", project, rm.Name)
	fmt.Fprintf(&sb, "  %s %s → %s\n", res.Branch, shortSHA(res.Old), shortSHA(res.New))
	return TermResult{
		Text: strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{
			"project": project, "remote": rm.Name, "refs": []gittransport.PushResult{res},
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
		return TermResult{}, fmt.Errorf("sync 需要 <project> [<remote>]")
	}
	if !s.projectVisibleFor(c, project) {
		return TermResult{}, fmt.Errorf("项目 %q 不在这张凭证的可见范围内", project)
	}
	rm, err := s.findRemote(project, name)
	if err != nil {
		return TermResult{}, err
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		return TermResult{}, err
	}

	remoteRefs, err := s.listRemote(rm)
	if err != nil {
		return TermResult{}, fmt.Errorf("读取 %s 的远端广告失败: %w", rm.Name, err)
	}

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
	fmt.Fprintf(&sb, "%s ↔ %s（%s）", project, rm.Name, gitremote.Redact(rm.URL))
	if pending > 0 {
		fmt.Fprintf(&sb, "  本地未提交 %d 处", pending)
	}
	sb.WriteString("\n")

	for _, b := range branches {
		local, _ := repo.Resolve(b)
		remote := remoteRefs["refs/heads/"+b]
		state := classifySync(repo, local, remote)
		rows = append(rows, row{Branch: b, Local: local, Remote: remote, State: state})
		fmt.Fprintf(&sb, "  %-16s %-12s 本地 %s 远端 %s\n", b, state, shortSHA(local), shortSHA(remote))
	}
	if len(rows) == 0 {
		sb.WriteString("（没有可读分支）")
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
			return nil, fmt.Errorf("读取 SSH 私钥 %q 失败: %w", rm.SSHKey, err)
		}
		return gittransport.ListSSH(ctx, rm.URL, string(key), rm.HostKey, remoteTimeout)
	case strings.HasPrefix(rm.URL, "http://"), strings.HasPrefix(rm.URL, "https://"):
		token, err := s.optionalSecret(rm.HTTPToken, "http_token")
		if err != nil {
			return nil, err
		}
		return gittransport.ListHTTP(ctx, rm.URL, rm.HTTPUser, token, remoteTimeout)
	}
	if _, ok := gittransport.LocalPath(rm.URL); ok {
		return gittransport.ListLocal(rm.URL)
	}
	return nil, fmt.Errorf("不支持的远端传输：%s", gitremote.Redact(rm.URL))
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

// projectVisibleFor 报告凭证能否看到该项目（任意分支可读即可）。
//
// 与 HTTP Git 端点用同一条判据：两个入口必须对「谁能看到这个项目」
// 给出同一个答案，否则受限凭证可以从弱的那一侧绕过去。
func (s *Server) projectVisibleFor(c Caller, project string) bool {
	repo, err := s.projects.Get(project)
	if err != nil {
		branch := s.persona
		if branch == "" {
			branch = "zero"
		}
		return s.allowed(c, project, branch, configBitRead)
	}
	refs, err := repo.Refs("refs/heads/")
	if err != nil {
		return false
	}
	if len(refs) == 0 {
		branch := s.persona
		if branch == "" {
			branch = "zero"
		}
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
