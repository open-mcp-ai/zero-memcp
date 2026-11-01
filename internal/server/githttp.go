package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
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

	// 分支级可见性无法在这里表达（git 协议按 ref 粒度），
	// 所以这里只检查「这个项目整体是否对该凭证可见」。
	if !s.projectVisible(caller, project) {
		http.Error(w, fmt.Sprintf("项目 %q 不在这张凭证的可见范围内", project), http.StatusForbidden)
		return
	}

	repo, err := s.projects.Get(project)
	if err != nil {
		http.Error(w, fmt.Sprintf("项目 %q: %v", project, err), http.StatusNotFound)
		return
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
			body, err := repo.AdvertiseRefsFiltered(s.persona, func(ref string) bool {
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
			if !s.gitPushAllowed(caller, project) {
				http.Error(w, "当前端口未开启 git-receive-pack，或凭证没有写权限", http.StatusForbidden)
				return
			}
			body, err := repo.AdvertiseReceivePack(s.persona, func(ref string) bool {
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
			http.Error(w, "不支持的 git 服务："+svc, http.StatusBadRequest)
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
		if !s.gitPushAllowed(caller, project) {
			http.Error(w, "当前端口未开启 git-receive-pack，或凭证没有写权限", http.StatusForbidden)
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
		body, err := repo.ReceivePack(in, func(ref string) bool {
			branch := strings.TrimPrefix(ref, "refs/heads/")
			return s.allowed(caller, project, branch, configBitWrite)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 写入成功后触发重载与脉冲广播：外部推进的 ref 也是真写入，
		// 内存索引与 SSE 锚定者必须立刻知道。
		_ = s.Reload()
		s.checkExternalUpdates()

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
		return "", "", fmt.Errorf("路径应为 /repos/<project>.git/<action>")
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
		return "", "", fmt.Errorf("未知的 git 动作：%s", rest)
	}
	project = strings.TrimSuffix(project, ".git")
	if project == "" {
		return "", "", fmt.Errorf("缺项目名")
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
		branch := s.persona
		if branch == "" {
			branch = "zero"
		}
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
func (s *Server) gitPushAllowed(c Caller, project string) bool {
	repo, err := s.projects.Get(project)
	if err != nil {
		return false
	}
	refs, _ := repo.Refs("refs/heads/")
	if len(refs) == 0 {
		return s.allowed(c, project, "zero", configBitWrite)
	}
	for _, ref := range refs {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if s.allowed(c, project, branch, configBitWrite) {
			return true
		}
	}
	return false
}
