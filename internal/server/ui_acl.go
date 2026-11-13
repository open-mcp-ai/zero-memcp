package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/acl"
)

// uiACLList 返回全部授权规则。
//
// 管理台只能读、不能写「凭证」——凭证是签发时的事实，事后改它就是伪造。
// 但「谁能碰哪条分支」是每天都会变的运行时数据，所以它必须能被改，
// 而且改完立刻生效。
func (s *Server) uiACLList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.aclActive() {
		uiJSON(w, map[string]any{
			"rules": []acl.Rule{},
			"error": "no ACL store on this instance (acl.json could not be opened); permissions fall back to credential mounts",
		})
		return
	}
	uiJSON(w, map[string]any{
		"rules": s.aclRulesForUI(),
		"path":  "system/acl.json",
	})
}

// uiACLPut 增加或覆盖一条规则。
func (s *Server) uiACLPut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.aclActive() {
		http.Error(w, "no ACL store on this instance", http.StatusBadRequest)
		return
	}
	var body struct {
		User    string `json:"user"`
		Project string `json:"project"`
		Branch  string `json:"branch"`
		Perm    string `json:"perm"`
		Admin   bool   `json:"admin"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rule := acl.Rule{
		User:    strings.TrimSpace(body.User),
		Project: strings.Trim(strings.TrimSpace(body.Project), "/"),
		Branch:  strings.TrimSpace(body.Branch),
		Perm:    strings.TrimSpace(body.Perm),
		Admin:   body.Admin,
		Note:    strings.TrimSpace(body.Note),
	}
	if rule.Branch == "*" {
		rule.Branch = ""
	}
	// 用户必须已声明：让一条指向不存在用户的规则进来，等于埋一个
	// 「配置看着生效、实际永远不匹配」的坑。
	if s.cfgIndex() != nil && rule.User != "" {
		if _, ok := s.cfgIndex().User(rule.User); !ok {
			http.Error(w, "user "+rule.User+" is not declared in [[user]] of the config", http.StatusBadRequest)
			return
		}
	}
	if !rule.Admin && rule.Project != "" && s.cfgIndex() != nil && !s.cfgIndex().Declared(rule.Project) {
		http.Error(w, "project "+rule.Project+" is not declared in the config", http.StatusBadRequest)
		return
	}
	saved, err := s.acl.Put(rule)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	uiJSON(w, map[string]any{"ok": true, "rule": saved})
}

// uiACLRm 删除一条规则。
func (s *Server) uiACLRm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.aclActive() {
		http.Error(w, "no ACL store on this instance", http.StatusBadRequest)
		return
	}
	var body struct {
		User    string `json:"user"`
		Project string `json:"project"`
		Branch  string `json:"branch"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.acl.Delete(body.User, body.Project, body.Branch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	uiJSON(w, map[string]any{"ok": true})
}
