package server

import (
	"embed"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

//go:embed static/index.html
var uiFS embed.FS

// handleIndex 提供极简、零外部依赖的记忆监控与检索面板。
//
// 面板只是观察窗口，不做任何写操作：记忆的所有权归服务端与仓库，
// 一个网页不该成为第二条修改通道。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := uiFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "面板资源缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	s.handleAPI(w, r)
}

// buildTreeFromBlocks 把一个项目的所有块组织成树形 JSON。
//
// 不需要碰磁盘：块的 Path 已经表达了整个目录结构（例如 "2026-10-30/xxx.md"）。
// 之前用 os.ReadDir 递归扫描目录，在裸仓库下是无效的（因为工作区根本没有平铺文件）。
func buildTreeFromBlocks(projectName string, blocks []memory.Block) map[string]any {
	type node struct {
		isDir    bool
		size     int64
		children map[string]*node
	}
	root := &node{isDir: true, children: map[string]*node{}}

	for _, b := range blocks {
		segs := strings.Split(b.Path, "/")
		cur := root
		for i, seg := range segs {
			isLast := i == len(segs)-1
			if cur.children[seg] == nil {
				cur.children[seg] = &node{isDir: !isLast, children: map[string]*node{}}
			}
			cur = cur.children[seg]
			if isLast {
				cur.size = b.Size
			}
		}
	}

	var walk func(name string, n *node) map[string]any
	walk = func(name string, n *node) map[string]any {
		if !n.isDir {
			return map[string]any{
				"name": name,
				"type": "file",
				"size": n.size,
			}
		}
		var children []map[string]any
		var keys []string
		for k := range n.children {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			children = append(children, walk(k, n.children[k]))
		}
		return map[string]any{
			"name":     name,
			"type":     "dir",
			"children": children,
		}
	}

	res := walk(projectName, root)
	res["path"] = projectName
	return res
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch r.URL.Query().Get("action") {
	case "recent":
		s.mu.RLock()
		var list []map[string]any
		for _, tr := range s.trees {
			if !s.allowed(caller, tr.Project, tr.Branch, configBitRead) {
				continue
			}
			for _, b := range tr.Blocks {
				virt := caller.Mount.ToVirtual(b.Project, b.Branch, b.Path)
				if virt == "" {
					continue
				}
				list = append(list, map[string]any{
					"addr":    virt,
					"size":    b.Size,
					"time":    time.Unix(b.Time, 0).Format("2006-01-02 15:04"),
					"modTime": b.Time,
				})
			}
		}
		s.mu.RUnlock()

		sort.Slice(list, func(i, j int) bool {
			return list[i]["modTime"].(int64) > list[j]["modTime"].(int64)
		})
		if len(list) > 25 {
			list = list[:25]
		}
		_ = json.NewEncoder(w).Encode(list)

	case "tree":
		s.mu.RLock()
		defer s.mu.RUnlock()

		var roots []map[string]any
		for _, tr := range s.trees {
			if !s.allowed(caller, tr.Project, tr.Branch, configBitRead) {
				continue
			}
			name := caller.Mount.ToVirtual(tr.Project, tr.Branch, "")
			if name == "" {
				name = tr.Project
			}
			roots = append(roots, buildTreeFromBlocks(name, tr.Blocks))
		}
		_ = json.NewEncoder(w).Encode(roots)

	case "read":
		addr := r.URL.Query().Get("addr")
		// 曾经这里硬编码 Caller{Root:true}：任何能通过鉴权的调用者
		// 都能从这个端点读到任意仓库的任意文件，把整个挂载表旁路掉。
		// 读路径的判据只能有一个——就是调用者本来的身份。
		out, err := s.execTerm("read "+addr, Viewer{}, caller)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"content": out.Text})

	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}
