package server

import (
	"embed"
	"encoding/json"
	"errors"
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
		http.Error(w, "admin panel assets are missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// buildTreeFromBlocks 把一个项目的所有块组织成树形 JSON。
//
// 不需要碰磁盘：块的 Path 已经表达了整个目录结构（例如 "2026-10-30/xxx.md"）。
// 之前用 os.ReadDir 递归扫描目录，在裸仓库下是无效的（因为工作区根本没有平铺文件）。
//
// addrPrefix 是叶子节点 addr 的前缀，形如 "weft@main.record"。
// 叶子节点的 addr = addrPrefix + ":" + 树内路径，正是
// /api?action=read 与 read 动词都接受的完整地址。
func buildTreeFromBlocks(projectName, addrPrefix string, blocks []memory.Block) map[string]any {
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

	// walk 把节点转成 JSON。filePath 从空串起算，文件节点带着
	// addrPrefix:path 的完整地址——前端 openFile 直接用它。
	// 之前只返回 name/size，前端拼 openFile('${n.addr}')
	// 时 n.addr 是 undefined，点树里任何文件都报
	// "got undefined"；搜索结果能读纯粹因为那条路径自带 addr。
	var walk func(name, filePath string, n *node) map[string]any
	walk = func(name, filePath string, n *node) map[string]any {
		if !n.isDir {
			addr := filePath
			if addrPrefix != "" {
				addr = addrPrefix + ":" + filePath
			}
			return map[string]any{
				"name": name,
				"type": "file",
				"size": n.size,
				"addr": addr,
			}
		}
		var children []map[string]any
		var keys []string
		for k := range n.children {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childPath := k
			if filePath != "" {
				childPath = filePath + "/" + k
			}
			children = append(children, walk(k, childPath, n.children[k]))
		}
		return map[string]any{
			"name":     name,
			"type":     "dir",
			"children": children,
		}
	}

	res := walk(projectName, "", root)
	res["path"] = projectName
	return res
}

// handleAPI 是 Agent 通道上的数据接口（Bearer 鉴权）。
//
// 管理台不用它——面板走 handleAPIAsUI，判据是会话。
// 两者共用同一段读取逻辑，但身份来源必须分开：一旦共用一个
// 鉴权函数，「本机免签降级成 root」就会顺着管理面漏进来。
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}
	s.serveAPI(w, r, caller)
}

// handleAPIAsUI 用管理台身份提供同一批数据。
//
// 管理台是所有者视角，所以身份是 root——但这不等于「可以绕过一切」：
// 它仍然只做读（recent / tree / read），没有任何写路径。
//
// 用户身份必须取配置里真实存在的 root 用户，不能自己编一个名字。
//
// 踩过的坑：这里原先把 User 写死成 "ui"，而配置里只有 zero/chat。
// allowed() 最后会走到 config.Check → Explain，那里查不到这个用户，
// 于是每次在面板里点开一个切片都得到：
//
//	Read failed: user "ui" does not exist
//
// 而 Caller.Root 为真并不能绕过这一步：root 只跳过「项目未声明」那一层，
// 已声明的项目仍然要查配置。所以直接复用 rootCaller()——它已经在
// 做「找配置里第一个 root 用户」这件事，身份标识也带上了 endpoint。
func (s *Server) handleAPIAsUI(w http.ResponseWriter, r *http.Request) {
	c := s.rootCaller()
	c.Identity = "webui"
	s.serveAPI(w, r, c)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request, caller Caller) {
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
			// 树是（项目, 分支）粒度的，但名字只用了项目名——于是一个有
			// 五条分支的项目在面板上出现五次，每次内容不同，却完全看不出
			// 哪棵是哪条分支。旧模型里 main.record 和 main 同时存在时，
			// 用户会看到两个 "weft"，一个装着 me/，另一个装着 record/。
			//
			// 根节点的名字带上分支，并把 addrPrefix 一并交给
			// buildTreeFromBlocks，让叶子节点的 addr 就是完整的
			// project@branch:path——这是 /api?action=read 能直接接受的
			// 唯一形式，也是用户从树上点开文件时唯一正确的地址。
			label := name + "@" + tr.Branch
			roots = append(roots, buildTreeFromBlocks(label, name+"@"+tr.Branch, tr.Blocks))
		}
		_ = json.NewEncoder(w).Encode(roots)

	case "read":
		addr := r.URL.Query().Get("addr")
		// Never build a DSL line out of request data.
		//
		// `"read " + addr` looks harmless until addr contains a newline:
		// the remainder is executed as another verb under this caller's
		// identity, so a read-only caller could reach a write verb. The
		// project rule is that payloads travel in vars/args and are never
		// concatenated into the command line; this endpoint has to obey the
		// same rule it demands of agents.
		text, err := s.readAddr(addr, caller)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"content": text})

	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// readAddr 按调用者身份读取一个地址，不经过 DSL 解析。
//
// 这是 /api?action=read 的实现。它直接走 resolveAddr + allowed + repo.Read，
// 与 read 动词共用同一套判定，但入口不接受命令文本——地址就是地址。
//
// 复用动词实现看似省事，代价是把「参数」和「命令」混成一层：
// 地址里出现换行或空格时，后半段会变成另一个动词，而权限判定发生在
// 动词解析之后，于是调用者能借自己的读权限触发写路径。
func (s *Server) readAddr(addr string, c Caller) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("addr is required")
	}
	// 地址是单个 token，出现空白说明它本来就不是一个地址。
	if strings.ContainsAny(addr, " \t\r\n") {
		return "", errors.New("addr must not contain whitespace")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	proj, br, path, err := s.resolveAddr(addr, c)
	if err != nil {
		return "", err
	}
	if !s.allowed(c, proj, br, configBitRead) {
		return "", errors.New(s.denyReason(c, proj, br, configBitRead))
	}
	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}
	d, err := repo.Read(br, path)
	if err != nil {
		return "", err
	}
	nowUnix := time.Now().Unix()
	s.weights.Bump(memory.MakeID(proj, br, path), nowUnix)
	s.weights.Bump(proj+"/"+path, nowUnix)
	return string(d), nil
}
