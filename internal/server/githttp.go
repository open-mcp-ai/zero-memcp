package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 只读 HTTP Git：让任何机器用原生 git 就能拉走整棵记忆树。
//
// 端点形态与标准 git smart HTTP 一致，所以 `git clone http://host:port/repos/<project>.git`
// 可以直接用，不需要装任何客户端插件。
//
//	GET  /repos/<project>.git/info/refs?service=git-upload-pack
//	POST /repos/<project>.git/git-upload-pack
//
// 为什么只做只读：边缘节点的职责是「被中心 pull」，它不该有接收推送的
// 入口。中心节点的写入走 MCP 动词，那条路有权限、有审计、有原子提交。
// 把 receive-pack 也开出来，等于在 MCP 之外多出一个绕过所有校验的
// 写入口——而两个入口的权限语义迟早会漂移。

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
		if svc != "git-upload-pack" {
			// receive-pack 一律拒绝：那是写入口，本服务不提供
			http.Error(w, "只支持 git-upload-pack（只读）", http.StatusForbidden)
			return
		}
		body, err := repo.AdvertiseRefs(s.persona)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)

	case "git-upload-pack":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// git 客户端会发 gzip 压缩的请求体
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
		body, err := repo.UploadPackResponse(in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
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
	if c.Mount.Empty() {
		return true
	}
	for _, p := range s.declaredProjects() {
		if p != project {
			continue
		}
		repo, err := s.projects.Get(p)
		if err != nil {
			return false
		}
		refs, _ := repo.Refs("refs/heads/")
		for _, ref := range refs {
			if c.Mount.Visible(p, strings.TrimPrefix(ref, "refs/heads/")) {
				return true
			}
		}
	}
	return false
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
