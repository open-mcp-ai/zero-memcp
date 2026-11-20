package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
)

// uiMasterKeysList 列出当前生效的主人公钥（仅在 WebUI 管理会话下可读）。
//
// 返回公钥文本、名字、来源（配置文件还是由谁派生）以及被授予的权限位。
// 绝不暴露任何私钥——服务端本来就没有私钥。
func (s *Server) uiMasterKeysList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var out []map[string]any
	if s.masterKeys != nil {
		ks := s.masterKeys.snapshot()
		out = make([]map[string]any, 0, len(ks))
		for _, k := range ks {
			caps := make([]string, 0, len(k.Caps))
			for c := range k.Caps {
				caps = append(caps, c)
			}
			sort.Strings(caps)
			out = append(out, map[string]any{
				"name":       k.Name,
				"public_key": decodePubToB64(k.Pub),
				"caps":       caps,
				"root":       k.Root,
				"source":     k.Source,
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": out, "count": len(out)})
}

// uiMasterKeyAdd 在 WebUI 里录入一把由根钥签发的新公钥。
//
// 为什么要求根钥签名：
// 即使 WebUI 管理员密码泄露，只要攻击者没有离线设备上的**根私钥**，
// 他也无法给自己添加一把带权限的主人公钥——两道门再次形成闭环。
func (s *Server) uiMasterKeyAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req keyRecord
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.addDelegatedKey(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":   true,
		"name": req.Name,
		"caps": req.Caps,
	})
}

// uiMasterKeyRevoke 吊销一把派生公钥（同样需要根钥签名作为凭据）。
func (s *Server) uiMasterKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req keyRecord
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.revokeDelegatedKey(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"revoked": req.Name,
	})
}

func decodePubToB64(pub []byte) string {
	if len(pub) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pub)
}
