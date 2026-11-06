package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 管理面数据接口。
//
// 这些动作以前只有 CLI 有（token mint/list/revoke、gc），现在搬到 WebUI：
// CLI 弱化成「起服务」一件事，而权限管理需要一个人类持有的会话——
// 它不该是 Agent 用一张 Bearer 就能顺手触发的。

// uiOverview 返回实例概况。
func (s *Server) uiOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	projects := map[string]int{}
	branches := map[string]int{}
	docs := 0
	for _, t := range s.trees {
		projects[t.Project] += len(t.Blocks)
		branches[t.Project+"@"+t.Branch]++
		docs += len(t.Indexable())
	}
	terms := s.idx.Terms()
	s.mu.RUnlock()

	pending := map[string]int{}
	for _, p := range s.declaredProjects() {
		repo, err := s.projects.Get(p)
		if err != nil {
			continue
		}
		if n := repo.Pending(); n > 0 {
			pending[p] = n
		}
	}

	uiJSON(w, map[string]any{
		"ok":             true,
		"version":        Version,
		"listen":         s.Addr(),
		"default_branch": s.defaultBranch,
		"docs":           docs,
		"terms":          terms,
		"projects":       projects,
		"branches":       branches,
		"pending":        pending,
		"must_change":    s.passwords != nil && s.passwords.MustChange(),
		"session_ttl":    s.uiSessionTTLText(),
		"sessions":       s.sessions.Count(),
		"enforce_auth":   s.cfg.EnforceAuth,
		"clock":          s.Clock(),
	})
}

// uiTokenView 是给界面看的凭证条目。
//
// Cap/Mount 的「未知」必须显式表达：旧记录没有这两个字段，
// 把它们显示成空串会被读成「不限制」，也就是「全权凭证」——
// 那是把最危险的结论当成默认值。
type uiTokenView struct {
	Name       string            `json:"name"`
	JTI        string            `json:"jti"`
	Sub        string            `json:"sub"`
	Exp        int64             `json:"exp"`
	Created    int64             `json:"created"`
	Expired    bool              `json:"expired"`
	Revoked    bool              `json:"revoked"`
	Cap        string            `json:"cap"`
	CapKnown   bool              `json:"cap_known"`
	Mount      []auth.MountPoint `json:"mount"`
	MountKnown bool              `json:"mount_known"`
	Gated      string            `json:"gated"`
	Grants     []auth.Grant      `json:"grants"`
}

// uiTokens 列出全部凭证（含能力与挂载，这是 CLI 时代缺的审计面）。
func (s *Server) uiTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	recs, errs := auth.LoadTokenRecords(s.dataDir)
	grantMap, _ := s.grants.ListAll()
	now := time.Now()

	out := make([]uiTokenView, 0, len(recs))
	for _, rec := range recs {
		v := uiTokenView{
			Name: rec.Name, JTI: rec.JTI, Sub: rec.Sub,
			Exp: rec.Exp, Created: rec.Created,
			Expired: rec.Expired(now),
			Revoked: s.rev.Revoked(rec.JTI),
			Cap:     rec.Cap, CapKnown: rec.CapRecorded,
			Mount: rec.Mount, MountKnown: rec.MountRecorded,
			Gated:  rec.Gated,
			Grants: grantMap[rec.JTI],
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	errText := make([]string, 0, len(errs))
	for _, e := range errs {
		errText = append(errText, e.Error())
	}
	uiJSON(w, map[string]any{"tokens": out, "errors": errText})
}

// uiTokenMint 签发一张凭证。
//
// 与 CLI 的 token mint 是同一套规则，只是入口换了：
// 用户名必须在配置里声明过、挂载点能力不能超出凭证能力、
// 记录里必须存下 cap/mount（否则下次又变成不可审计）。
func (s *Server) uiTokenMint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		User         string `json:"user"`
		Name         string `json:"name"`
		Cap          string `json:"cap"`
		ExpiresHours int    `json:"expires_hours"`
		Gated        string `json:"gated"`
		Mounts       []struct {
			Project string `json:"project"`
			At      string `json:"at"`
			Branch  string `json:"branch"`
			Path    string `json:"path"`
			Cap     string `json:"cap"`
		} `json:"mounts"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	user := strings.TrimSpace(body.User)
	if user == "" {
		http.Error(w, "user is required", http.StatusBadRequest)
		return
	}
	if s.cfgIndex() == nil {
		http.Error(w, "no config file; cannot verify whether the user exists", http.StatusBadRequest)
		return
	}
	if _, ok := s.cfgIndex().User(user); !ok {
		http.Error(w, fmt.Sprintf("user %q is not declared in [[user]] in the config", user), http.StatusBadRequest)
		return
	}
	credName := strings.TrimSpace(body.Name)
	if credName == "" {
		credName = user
	}
	if err := auth.ValidateName(credName); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	capStr := strings.TrimSpace(body.Cap)
	if capStr == "" {
		http.Error(w, "cap is required; use rwx explicitly for unrestricted capability", http.StatusBadRequest)
		return
	}
	capStr, err := auth.NormalizeCapabilitySet(capStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// gated must be a subset of cap: you cannot hold a bit the credential
	// never had. Anything else would make the admin panel a place where a
	// typo silently invents capability.
	gated, gerr := auth.NormalizeCapabilitySet(body.Gated)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusBadRequest)
		return
	}
	for _, bit := range gated {
		if capStr != "" && !strings.ContainsRune(capStr, bit) {
			http.Error(w, fmt.Sprintf("gated bit %q is not part of capability %q; a gated bit can only withhold a capability the credential already has", string(bit), capStr), http.StatusBadRequest)
			return
		}
	}

	var mounts []auth.MountPoint
	for _, m := range body.Mounts {
		pt := auth.MountPoint{
			Project: strings.TrimSpace(m.Project),
			At:      strings.TrimSpace(m.At),
			Branch:  strings.TrimSpace(m.Branch),
			Path:    strings.TrimSpace(m.Path),
			Cap:     strings.TrimSpace(m.Cap),
		}
		if pt.Project == "" {
			continue
		}
		if pt.At == "" {
			pt.At = "/"
		}
		if err := auth.ValidateRepoName(pt.Project); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// 与签发规则同源：挂载点能力必须是凭证能力的子集。
		if err := auth.ValidateMountCap(capStr, pt); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mounts = append(mounts, pt)
	}
	if len(mounts) == 0 {
		http.Error(w, "at least one mount is required; an empty mount table means all namespaces", http.StatusBadRequest)
		return
	}

	jti, err := auth.NewJTI()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var expAt int64
	if body.ExpiresHours > 0 {
		expAt = time.Now().Add(time.Duration(body.ExpiresHours) * time.Hour).Unix()
	}
	token, err := s.tokens.Sign(auth.Claims{
		Sub:   user,
		Name:  credName,
		Exp:   expAt,
		JTI:   jti,
		Cap:   capStr,
		Mount: mounts,
		Gated: gated,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 记录里必须带上 cap/mount：不然这张凭证签发完就又是「不可审计」的。
	if err := auth.SaveTokenRecord(s.dataDir, auth.TokenRecord{
		Name:          credName,
		JTI:           jti,
		Sub:           user,
		Exp:           expAt,
		Created:       time.Now().Unix(),
		Cap:           capStr,
		Mount:         mounts,
		CapRecorded:   true,
		MountRecorded: true,
		Gated:         gated,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	uiJSON(w, map[string]any{"ok": true, "name": credName, "jti": jti, "token": token, "exp": expAt})
}

// uiTokenRevoke 吊销或恢复一张凭证。
func (s *Server) uiTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		JTI  string `json:"jti"`
		Undo bool   `json:"undo"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.JTI) == "" {
		http.Error(w, "jti is required", http.StatusBadRequest)
		return
	}
	if body.Undo {
		if err := s.rev.Unrevoke(body.JTI); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		uiJSON(w, map[string]any{"ok": true, "jti": body.JTI, "revoked": false})
		return
	}
	if err := s.rev.Revoke(body.JTI); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 吊销后连带清掉它的临时授权：留着只会让「已吊销」这张纸在
	// 某天被 unrevoke 时莫名其妙带着旧的写权限回来。
	_, _ = s.grants.Revoke(body.JTI, "", "")
	uiJSON(w, map[string]any{"ok": true, "jti": body.JTI, "revoked": true})
}

// uiGrants 列出全部临时写授权。
func (s *Server) uiGrants(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	all, err := s.grants.ListAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recs, _ := auth.LoadTokenRecords(s.dataDir)
	names := map[string]string{}
	gr := map[string]string{}
	for _, rec := range recs {
		names[rec.JTI] = rec.Name
		gr[rec.JTI] = rec.Gated
	}
	type row struct {
		auth.Grant
		Name    string `json:"name"`
		Gated   string `json:"gated"`
		Expired bool   `json:"expired"`
	}
	now := time.Now()
	var rows []row
	for jti, list := range all {
		for _, g := range list {
			rows = append(rows, row{Grant: g, Name: names[jti], Gated: gr[jti], Expired: g.Expired(now)})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		if rows[i].Project != rows[j].Project {
			return rows[i].Project < rows[j].Project
		}
		return rows[i].Branch < rows[j].Branch
	})
	uiJSON(w, map[string]any{"grants": rows})
}

// uiGrantAdd 给某张凭证在某个分支上临时开写权限。
func (s *Server) uiGrantAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		JTI     string `json:"jti"`
		Project string `json:"project"`
		Branch  string `json:"branch"`
		Cap     string `json:"cap"`
		Minutes int    `json:"minutes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	jti := strings.TrimSpace(body.JTI)
	project := strings.TrimSpace(body.Project)
	branch := strings.TrimSpace(body.Branch)
	if jti == "" || project == "" {
		http.Error(w, "jti and project are required", http.StatusBadRequest)
		return
	}
	if branch == "" {
		branch = s.defaultBranch
	}
	if err := auth.ValidateRepoName(project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if branch != "*" {
		if err := auth.ValidateBranchName(branch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// 授权必须落在该凭证本来就能碰到的范围内。
	//
	// 这不是多余的一步：授权是「临时打开一道闸门」，不是「发一张新凭证」。
	// 如果它能突破 cap/mount，那 WebUI 就成了提权工具，
	// 而整套「凭证只能收窄」的模型当场作废。
	recs, _ := auth.LoadTokenRecords(s.dataDir)
	var rec *auth.TokenRecord
	for i := range recs {
		if recs[i].JTI == jti {
			rec = &recs[i]
			break
		}
	}
	if rec == nil {
		http.Error(w, "no issuance record found for this credential", http.StatusNotFound)
		return
	}
	grantCap, err := auth.NormalizeCapabilitySet(strings.TrimSpace(body.Cap))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if grantCap == "" {
		http.Error(w, "cap is required (choose one or more of r, w, x)", http.StatusBadRequest)
		return
	}
	// A grant may only open bits that this credential explicitly holds back.
	//
	// The credential cap is the ceiling; its mount cap can narrow that ceiling
	// per project/branch; Gated says which remaining bits require activation.
	// The grant itself must be a subset of all three. Config/TOML is still
	// checked by allowed() afterwards, so a grant can never override it.
	if rec.Gated == "" {
		http.Error(w, "this credential has no gated capabilities; set gated when minting it before requesting temporary grants", http.StatusBadRequest)
		return
	}
	mountCap, covered := mountCapFor(rec.Mount, rec.Cap, project, branch)
	if !covered {
		http.Error(w, fmt.Sprintf("project %s@%s is outside this credential's mount table; a grant cannot widen the mount scope", project, branch), http.StatusBadRequest)
		return
	}
	for _, bit := range grantCap {
		// An empty capability set means "unrestricted at this layer", which is the
		// documented meaning everywhere else in this model. Membership checks only
		// apply when that layer explicitly narrows the set.
		if rec.Cap != "" && !strings.ContainsRune(rec.Cap, bit) {
			http.Error(w, fmt.Sprintf("grant bit %q exceeds credential cap %q", string(bit), rec.Cap), http.StatusBadRequest)
			return
		}
		if mountCap != "" && !strings.ContainsRune(mountCap, bit) {
			http.Error(w, fmt.Sprintf("grant bit %q exceeds the mount cap %q for %s@%s", string(bit), mountCap, project, branch), http.StatusBadRequest)
			return
		}
		if !strings.ContainsRune(rec.Gated, bit) {
			http.Error(w, fmt.Sprintf("grant bit %q is not gated on this credential; gated bits are %q", string(bit), rec.Gated), http.StatusBadRequest)
			return
		}
	}
	if s.rev.Revoked(jti) {
		http.Error(w, "this credential has been revoked", http.StatusBadRequest)
		return
	}
	if rec.Expired(time.Now()) {
		http.Error(w, "this credential has expired", http.StatusBadRequest)
		return
	}

	var expires int64
	if body.Minutes > 0 {
		expires = time.Now().Add(time.Duration(body.Minutes) * time.Minute).Unix()
	}
	if err := s.grants.Grant(auth.Grant{
		JTI: jti, Project: project, Branch: branch, Cap: grantCap,
		Expires: expires, GrantedBy: "webui",
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	uiJSON(w, map[string]any{"ok": true, "jti": jti, "project": project, "branch": branch, "cap": grantCap, "expires": expires})
}

// uiGrantRevoke 撤销临时授权。
func (s *Server) uiGrantRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		JTI     string `json:"jti"`
		Project string `json:"project"`
		Branch  string `json:"branch"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.JTI) == "" {
		http.Error(w, "jti is required", http.StatusBadRequest)
		return
	}
	n, err := s.grants.Revoke(strings.TrimSpace(body.JTI), strings.TrimSpace(body.Project), strings.TrimSpace(body.Branch))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	uiJSON(w, map[string]any{"ok": true, "removed": n})
}

// uiGC 触发一次对象打包。
//
// 与后台维护的 Repack 是同一个动作。放在管理面而不是 CLI：
// 它需要与活服务互斥，而管理面本来就在服务进程内——不存在第二个
// 进程同时改同一批仓库的问题。
func (s *Server) uiGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type result struct {
		Project string `json:"project"`
		Objects int    `json:"objects"`
		Removed int    `json:"removed"`
		Error   string `json:"error,omitempty"`
	}
	var out []result
	for _, p := range s.declaredProjects() {
		repo, err := s.projects.Get(p)
		if err != nil {
			out = append(out, result{Project: p, Error: err.Error()})
			continue
		}
		res, rerr := repo.Repack()
		row := result{Project: p, Objects: res.Objects, Removed: res.Removed}
		if rerr != nil {
			row.Error = rerr.Error()
		}
		out = append(out, row)
	}
	uiJSON(w, map[string]any{"ok": true, "results": out})
}

// uiSearch 供管理台检索。
//
// 走管理面接口而不是复用 /recall：/recall 是 Agent 通道的入口，
// 它按 Bearer 凭证的挂载表过滤。管理台是全量视图，
// 混用会让「面板看到的东西取决于当前带的是哪张 token」这种怪事。
func (s *Server) uiSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		uiJSON(w, map[string]any{"hits": []any{}})
		return
	}
	s.mu.RLock()
	hits := s.idx.SearchWeighted(q, 20, 0, s.weightCount)
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		snip := s.snippet(h.Addr, q, 160)
		out = append(out, map[string]any{
			"addr":    h.Addr,
			"score":   h.Score,
			"snippet": snip,
		})
	}
	s.mu.RUnlock()
	uiJSON(w, map[string]any{"hits": out})
}

// TakeBootstrapPassword returns and clears the one-time bootstrap password.
//
// A plain getter would leave the secret retrievable for the entire process
// lifetime. The CLI needs one read to print it; that read consumes it.
func (s *Server) TakeBootstrapPassword() string {
	pw := s.cfg.UIBootstrapPassword
	s.cfg.UIBootstrapPassword = ""
	return pw
}

// UIRoutesEnabled reports whether this instance serves the admin surface.
//
// It is always true: the admin surface is not optional. A deployment where
// nobody can reach it - a container, an npm wrapper, a daemon - has no other
// way to change the initial password, mint a credential, or grant a gated
// capability bit, so "off" would just mean "locked out". What the config can
// choose is *which* port carries it ([[listen]] webui = true); the default port
// always does.
func (s *Server) UIRoutesEnabled() bool {
	return true
}

// AdminURLs returns the addresses that serve the admin surface.
//
// Printed at startup so nobody has to guess which port to open, and so a
// deployment that moved the surface to a dedicated listener says so out loud.
func (s *Server) AdminURLs() []string {
	var out []string
	blocks := s.listens
	if len(blocks) == 0 {
		blocks = []ListenBlock{{Addr: s.cfg.Listen, WebUI: true}}
	}
	for _, b := range blocks {
		if !b.WebUI {
			continue
		}
		addr := b.Addr
		// A wildcard bind is not something you can paste into a browser.
		if host, port, err := net.SplitHostPort(addr); err == nil {
			switch host {
			case "", "0.0.0.0":
				addr = net.JoinHostPort("127.0.0.1", port)
			case "::":
				addr = net.JoinHostPort("::1", port)
			}
		}
		out = append(out, "http://"+addr+"/")
	}
	return out
}

// mountCapFor returns the effective credential/mount capability ceiling for a
// project and branch. The bool is false when an explicit mount table does not
// cover the requested pair. An empty cap means unrestricted at that layer.
func mountCapFor(mounts []auth.MountPoint, credentialCap, project, branch string) (string, bool) {
	if len(mounts) == 0 {
		return credentialCap, true
	}
	for _, m := range mounts {
		if strings.Trim(m.Project, "/") != strings.Trim(project, "/") {
			continue
		}
		if m.Branch != "" && m.Branch != "*" && m.Branch != branch {
			continue
		}
		if m.Cap == "" {
			return credentialCap, true
		}
		if credentialCap == "" {
			return m.Cap, true
		}
		var intersection strings.Builder
		for _, bit := range "rwx" {
			if strings.ContainsRune(credentialCap, bit) && strings.ContainsRune(m.Cap, bit) {
				intersection.WriteRune(bit)
			}
		}
		return intersection.String(), true
	}
	return "", false
}
