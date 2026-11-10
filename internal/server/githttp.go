package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// HTTP Git 端点：支持 clone/pull（git-upload-pack）与 push（git-receive-pack）。
//
// 端点形态与标准 git smart HTTP 一致，所以 `git clone http://host:port/repos/<project>.git`
// 可以直接用，不需要装任何客户端插件。
//
//	GET  /repos/<project>.git/info/refs?service=git-upload-pack
//	POST /repos/<project>.git/git-upload-pack
//
// push 的安全防线：
//  1. 必须在 [[listen]] 的 allow 里显式声明 "git-receive-pack"（缺省不给）；
//  2. 凭证必须有对应分支的写权限（allowed bit: w）；
//  3. 只接受快进，非快进一律拒绝；
//  4. 落地后触发 Reload 与树脉冲通知，不会变成静默写入。

// handleGitHTTP 是 /repos/ 前缀的统一入口。
func (s *Server) handleGitHTTP(w http.ResponseWriter, r *http.Request) {
	// 兼容「token 写在 URL 里」的三种形态：
	//   /repos/x.git?token=.../info/refs   （git 客户端自己拼的）
	//   /repos/x.git/info/refs?service=...&token=...
	//   /repos/x.git/info/refs?token=...
	// 三种都剥出 token 塞进 header，然后用干净路径继续。
	if tok := extractURLToken(r); tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	project, action, err := parseGitPathFull(fullPath(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// 鉴权与 MCP 同源：Bearer 或 ?token=
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		// 同时声明 Basic 与 Bearer：git 客户端默认走 Basic，
		// 只发 Bearer 它会直接放弃重试；程序消费者用 Bearer。
		w.Header().Add("WWW-Authenticate", `Basic realm="zero-memcp"`)
		w.Header().Add("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}

	// 存在性、可见性、可建性三件事要分开判断。
	//
	// 1. 库已有 → 走可见性检查（表外 403）
	// 2. 库还没有 → 读取一律 404（「对端还没写过东西」不能被报成没权限，
	//    这个字之差是一整轮跑偏的排查）；推送可以建库，但只限
	//    配置里声明过的项目或无限制的 root，否则任何人推一个名字就能
	//    在服务端造出一个空仓库。
	//
	// 推送方向要放行**两个**请求，不只一个：
	//
	//   GET  /info/refs?service=git-receive-pack   探测（git 先发这个）
	//   POST /git-receive-pack                     真正推送
	//
	// 只放行后者的话，客户端在探测那步就拿到 404，永远走不到推送——
	// 症状是「向空项目推第一次」直接失败并报「仓库不存在」，而配置里
	// 明明声明了这个项目。实测踩到过：修了 Ensure 仍然推不上去。
	receiveDir := action == "git-receive-pack" ||
		(action == "info/refs" && r.URL.Query().Get("service") == "git-receive-pack")

	repo, getErr := s.projects.Get(project)
	if getErr != nil {
		if !receiveDir {
			http.Error(w, fmt.Sprintf("project %q has no repository yet", project), http.StatusNotFound)
			return
		}
		if !s.projectDeclared(project) && !s.rootUnrestricted(caller) {
			http.Error(w, fmt.Sprintf("project %q has no repository yet", project), http.StatusNotFound)
			return
		}
		var ensErr error
		repo, ensErr = s.projects.Ensure(project)
		if ensErr != nil {
			http.Error(w, fmt.Sprintf("project %q: %v", project, ensErr), http.StatusInternalServerError)
			return
		}
	} else {
		// 分支级可见性无法在这里表达（git 协议按 ref 粒度），
		// 所以这里只检查「这个项目整体是否对该凭证可见」。
		//
		// 推送方向不套这道门：projectVisible 是**读**门，要求「能读该
		// 项目的每一条分支」。推送只需要「能写它要推的那几条」，而那几条
		// 已经由 ReceivePack 的 per-ref 回调逐条裁决了。把读门套在推送上
		// 还有两个坏处：
		//   1. 刚由 Ensure 建出来的空仓库没有分支，这道门会退回拿服务端
		//      默认分支（zero）去问，把只挂载了 main 的凭证直接判死；
		//   2. 一张「只写不读」的凭证本来就能推，套上读门反而拦掉它。
		if !receiveDir && !s.projectVisible(caller, project) {
			http.Error(w, fmt.Sprintf("project %q is outside this credential's visible scope", project), http.StatusForbidden)
			return
		}
	}

	switch action {
	case "info/refs":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		svc := r.URL.Query().Get("service")
		switch svc {
		case "git-upload-pack":
			body, err := repo.AdvertiseRefsFiltered(s.defaultBranch, func(ref string) bool {
				branch := strings.TrimPrefix(ref, "refs/heads/")
				if branch == ref {
					return true
				}
				return s.allowed(caller, project, branch, configBitRead)
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)

		case "git-receive-pack":
			if !s.gitPushAllowedRepo(caller, project, repo) {
				http.Error(w, "git-receive-pack is not enabled on this port, or the credential lacks write permission", http.StatusForbidden)
				return
			}
			body, err := repo.AdvertiseReceivePack(s.defaultBranch, func(ref string) bool {
				branch := strings.TrimPrefix(ref, "refs/heads/")
				return s.allowed(caller, project, branch, configBitWrite)
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)

		default:
			http.Error(w, "unsupported git service: "+svc, http.StatusBadRequest)
		}

	case "git-upload-pack":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		in := r.Body
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			gz, err := newGzipReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer gz.Close()
			in = gz
		}
		allowedTips := map[string]bool{}
		refs, _ := repo.Refs("refs/heads/")
		for _, ref := range refs {
			branch := strings.TrimPrefix(ref, "refs/heads/")
			if !s.allowed(caller, project, branch, configBitRead) {
				continue
			}
			sha, _ := repo.Resolve(branch)
			if sha != "" {
				allowedTips[sha] = true
			}
		}
		if tags, err := repo.Tags(); err == nil {
			for _, sha := range tags {
				allowedTips[sha] = true
			}
		}
		body, err := repo.UploadPackResponseFiltered(in, func(sha string) bool { return allowedTips[sha] })
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)

	case "git-receive-pack":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.gitPushAllowedRepo(caller, project, repo) {
			http.Error(w, "git-receive-pack is not enabled on this port, or the credential lacks write permission", http.StatusForbidden)
			return
		}
		in := r.Body
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			gz, err := newGzipReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer gz.Close()
			in = gz
		}
		// 收之前的 ref 快照：后面用它判断这次推送到底有没有带来新东西。
		//
		// 这是「流言止于智者」的判据。重推一个已存在的提交是很常见的
		// （重试、脚本重复执行），那种情况下 ref 不会推进，此时再扇出
		// 一轮 fetch/merge/push 只是白跑一个完整网络往返。
		before := projectHeads(repo)

		body, err := repo.ReceivePack(in, func(ref string) bool {
			branch := strings.TrimPrefix(ref, "refs/heads/")
			return s.allowed(caller, project, branch, configBitWrite)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 写入成功后触发重载与脉冲广播：外部推进的 ref 也是真写入，
		// 内存索引与锚定者必须立刻知道。
		_ = s.Reload()
		s.checkExternalUpdates()

		// 只把「真的推进了」的分支拿出去扇出。
		//
		// 因为历史是 Merkle 链，「这条消息我见过没有」可以退化成一个
		// SHA 判断：知道头就知道头以下的一切。所以不需要消息 ID 集合
		// 或 Bloom filter，比对前后头即可。
		for _, adv := range advancedRefs(before, projectHeads(repo)) {
			s.triggerFanout(project, adv.Branch, adv.SHA, "received push")
		}

		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	default:
		http.Error(w, "unsupported git service", http.StatusNotFound)
	}
}

// parseGitPath 从 /repos/<project>.git/<action> 里取出项目与动作。
func parseGitPath(p string) (project, action string, err error) {
	rest := strings.TrimPrefix(p, "/repos/")
	if rest == p || rest == "" {
		return "", "", fmt.Errorf("path must be /repos/<project>.git/<action>")
	}
	// 动作在后，项目名可能含斜杠
	for _, act := range []string{"info/refs", "git-upload-pack", "git-receive-pack"} {
		if strings.HasSuffix(rest, "/"+act) {
			project = strings.TrimSuffix(rest, "/"+act)
			action = act
			break
		}
	}
	if action == "" {
		return "", "", fmt.Errorf("unknown git action: %s", rest)
	}
	project = strings.TrimSuffix(project, ".git")
	if project == "" {
		return "", "", fmt.Errorf("project name is required")
	}
	return project, action, nil
}

// projectVisible 报告该凭证能否看到这个项目（任意分支可见即可）。
func (s *Server) projectVisible(c Caller, project string) bool {
	// Smart Git advertises and serves the repository as a whole; it cannot enforce
	// per-branch ACLs. Fail closed unless the caller may read every active branch.
	repo, err := s.projects.Get(project)
	if err != nil {
		return false
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
		if !s.allowed(c, project, branch, configBitRead) {
			return false
		}
	}
	return true
}

func newGzipReader(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) }

// extractURLToken 从任何位置的 query 里取出 token。
//
// git 客户端会把 ?token=... 拼在它自己构造的 URL 上，位置不可控：
// clone 时它在仓库名后面，走协议时它可能跑到动作路径中间。
// 所以这里按「整条原始请求里出现过的 token= 参数」来抓，
// 而不是假设它一定在 ?service= 旁边。
func extractURLToken(r *http.Request) string {
	if v := r.URL.Query().Get("token"); v != "" {
		return v
	}
	raw := r.URL.Path + "?" + r.URL.RawQuery
	for {
		i := strings.Index(raw, "token=")
		if i < 0 {
			return ""
		}
		rest := raw[i+len("token="):]
		if j := strings.IndexAny(rest, "/&?#"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
}

// fullPath 把 Path 和 RawQuery 拼回客户端真正请求的样子。
//
// 需要这么做是因为 git 客户端会把 ?token=... 拼在仓库名后面，
// 再往后接 /info/refs，于是它请求的是：
//
//	/repos/t.git?token=abc/info/refs
//
// Go 的 url.Parse 会把它拆成 Path=/repos/t.git、RawQuery=token=abc/info/refs，
// 动作被吞进了 query。这里把它拼回去再解析。
func fullPath(r *http.Request) string {
	p := r.URL.Path
	q := r.URL.RawQuery
	if q == "" {
		return p
	}
	// query 里带 / 说明是误拼进路径的动作部分
	if strings.Contains(q, "/") {
		return p + "?" + q
	}
	return p
}

// parseGitPathFull 处理可能带 ?query 的完整路径。
func parseGitPathFull(p string) (project, action string, err error) {
	if q := strings.Index(p, "?"); q >= 0 {
		head, tail := p[:q], p[q+1:]
		// tail 形如 token=abc/info/refs：把动作部分切出来接到 head 上
		if i := strings.Index(tail, "/"); i >= 0 {
			tail = tail[i:]
		} else {
			tail = ""
		}
		// tail 里还可能带 &service=... 之类的参数，切掉
		if j := strings.Index(tail, "&"); j >= 0 {
			tail = tail[:j]
		}
		p = head + tail
	}
	return parseGitPath(p)
}

// gitPushAllowed 检查凭证侧：该项目下至少有一条它能写的分支。
// 端口侧的门槛在 portPolicy 里，两处都要过。
func (s *Server) gitPushAllowedRepo(c Caller, project string, repo *gitstore.Repo) bool {
	if repo == nil {
		return false
	}
	refs, _ := repo.Refs("refs/heads/")
	if len(refs) == 0 {
		// 空仓库：没有任何已有分支可以拿来问。
		//
		// 不能拿 s.defaultBranch 去问——那是**服务端**的默认分支（zero），
		// 与这张凭证被授权写哪些分支无关。一张只挂载了 main/main.record
		// 的凭证，向空仓库推 main 是完全正当的，却会因为「它不能写 zero」
		// 被判 403。实测踩到过：自举时 spoke 全量推不上去，报
		// "credential lacks write permission"。
		//
		// 正确的门槛是「这个项目下至少有一条它算写的分支」，逐条问挂载表
		// 里列出的分支即可；具体哪条分支允许落，由 ReceivePack 的
		// per-ref allowRef 回调逐条裁决，能力不会因此放大。
		if s.allowed(c, project, s.defaultBranch, configBitWrite) {
			return true
		}
		for _, branch := range c.Mount.BranchesOf(project) {
			if branch == "" || branch == "*" {
				continue
			}
			if s.allowed(c, project, branch, configBitWrite) {
				return true
			}
		}
		return c.Mount.Empty() && s.allowed(c, project, s.defaultBranch, configBitWrite)
	}
	for _, ref := range refs {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if s.allowed(c, project, branch, configBitWrite) {
			return true
		}
	}
	return false
}

// projectHeads 取某个仓库当前所有一级分支的头。
//
// 只取 refs/heads/ 下不带斜杠的一级分支名，与其他同步路径的口径一致
// （sync_loop 里同样只处理一级分支）。
func projectHeads(repo *gitstore.Repo) map[string]string {
	out := map[string]string{}
	refs, err := repo.Refs("refs/heads/")
	if err != nil {
		return out
	}
	for _, ref := range refs {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if branch == ref || branch == "" || strings.Contains(branch, "/") {
			continue
		}
		if sha, err := repo.Resolve(branch); err == nil && sha != "" {
			out[branch] = sha
		}
	}
	return out
}

// advancedRef 是一条真正推进了的分支。
type advancedRef struct {
	Branch string
	SHA    string
}

// advancedRefs 比对前后快照，返回真的推进了的分支。
//
// 三种情况都算推进：
//
//	新增分支          before 没有，after 有
//	分支前进          before 与 after 不同
//
// 分支被删除（before 有、after 没有）不算推进——没有新消息可传播，
// 而删除本身在这个系统里是被禁止的（记忆历史只追加）。
func advancedRefs(before, after map[string]string) []advancedRef {
	var out []advancedRef
	for branch, sha := range after {
		if before[branch] != sha {
			out = append(out, advancedRef{Branch: branch, SHA: sha})
		}
	}
	// 顺序稳定，便于日志与测试
	sort.Slice(out, func(i, j int) bool { return out[i].Branch < out[j].Branch })
	return out
}

// projectDeclared 报告某项目是否在配置里显式声明过。
//
// 它与「磁盘上有这个目录」不是一回事：声明是意图，目录是事实。
// 接收推送时只信意图——配置里写了才算「这个项目该存在」，
// 否则任何人推一个名字就能在服务端造出一个空仓库。
func (s *Server) projectDeclared(project string) bool {
	for _, p := range s.declaredProjects() {
		if p == project {
			return true
		}
	}
	return false
}
