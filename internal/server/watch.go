package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 关注标记（remind）：调用方声明「我关心这棵树」。
//
// # 它不再是订阅，也不推送任何东西
//
// 早先这里挂着一整套推送：remind 锚定 → 服务端广播 tree 事件 → 客户端
// 长连接接收。那套东西自造了一条协议（自定义 event 名、自定义帧格式、
// 自定义鉴权语义），而本服务对外承诺的是 MCP。多一条推送通道意味着
// 多一份要维护的传输实现，而它带来的只是「少查一次」。
//
// 现在 remind 只做一件事：记下「谁关心哪棵树，以及记下时的分支头」。
// 变更感知靠调用方主动查：
//
//	status            → 报出关注的树现在有没有往前跑（对比锚定时记录的头）
//	since <t> <sha>   → 真有变化时取差量
//	remind <t>        → 重新锚定，把基线推到当前
//
// 三个都是 term 动词，走同一个 /mcp 入口。没有第二条通道。
//
// # 为什么记录基线而不是只记「我关心」
//
// 不记基线就只能回答「你在关注这棵树」，回答不了「它变了没有」——
// 而后者才是关注的目的。基线让 status 能给出可执行的信息，
// 且不需要任何后台任务或定时器。

// TreeWatch 是一条关注标记。
type TreeWatch struct {
	Project string `json:"project"`
	Branch  string `json:"branch"`
	// Path 为空表示关注整个分支。
	Path string `json:"path"`
	// Since 是锚定那一刻的分支头。
	//
	// 它是「上次看到哪」的锚点：status 拿它和当前头比对，就能回答
	// 「我关注的东西动了没有」，而服务端不需要记录任何额外状态。
	Since string `json:"since,omitempty"`
}

// TreeWatchHub 保存各身份的关注标记。
//
// 键是 identity（由凭证决定）而不是连接：这里没有连接概念了。
// 关注标记是「这个人关心什么」，属于身份属性，与第几次调用的生命周期无关。
type TreeWatchHub struct {
	mu      sync.RWMutex
	watches map[string][]TreeWatch
	// path 非空时把整张表落盘。
	//
	// 为什么必须落盘：关注标记是「我关心这棵树」这个判断本身，属于身份，
	// 不属于进程生命周期。纯内存的代价是每次重启后两个分身都「忘了自己在
	// 关注共享分支」，而它们并不会自己重新 remind——于是对讲机静默失效，
	// 症状是「消息发出去了但对面一直没醒」，没有任何报错。
	//
	// 空 path 表示不持久化（测试与单机临时实例）。
	path string
}

func newTreeWatchHub() *TreeWatchHub {
	return &TreeWatchHub{watches: make(map[string][]TreeWatch)}
}

// newPersistentTreeWatchHub 打开一份落盘的关注表。文件不存在视为空表。
func newPersistentTreeWatchHub(path string) (*TreeWatchHub, error) {
	h := &TreeWatchHub{watches: make(map[string][]TreeWatch), path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return h, nil
	}
	var doc struct {
		Watches map[string][]TreeWatch `json:"watches"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 读坏了不能拖垮启动：关注是派生态，丢一次最多重读一遍树。
		// 但要出声——静默变空表会让「remind 明明做过」变成一次无从查起的谜。
		return h, fmt.Errorf("parse %s: %w (starting with an empty watch table)", path, err)
	}
	if doc.Watches != nil {
		h.watches = doc.Watches
	}
	return h, nil
}

// saveLocked 把当前表写回磁盘（调用方已持锁）。
//
// 写失败不回滚内存：关注丢了只是多读一次树，而把内存改回旧值会让
// 调用方刚做完的 remind 看起来「成功了但没生效」。日志里出声即可。
func (h *TreeWatchHub) saveLocked() {
	if h.path == "" {
		return
	}
	body, err := json.MarshalIndent(struct {
		Watches map[string][]TreeWatch `json:"watches"`
	}{Watches: h.watches}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(h.path), ".watches-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return
	}
	_ = tmp.Sync()
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(name, h.path)
}

// Add 为某个身份增加或更新一条关注标记（同 project+branch+path 视为同一条）。
func (h *TreeWatchHub) Add(identity string, w TreeWatch) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.watches[identity]
	for i, cur := range list {
		if cur.Project == w.Project && cur.Branch == w.Branch && cur.Path == w.Path {
			// 重新锚定：把基线推到当前头。
			list[i] = w
			h.saveLocked()
			return
		}
	}
	h.watches[identity] = append(list, w)
	h.saveLocked()
}

// List 返回该身份当前关注的树。
func (h *TreeWatchHub) List(identity string) []TreeWatch {
	h.mu.RLock()
	defer h.mu.RUnlock()
	src := h.watches[identity]
	out := make([]TreeWatch, len(src))
	copy(out, src)
	return out
}

// Clear 清空该身份的全部关注。
func (h *TreeWatchHub) Clear(identity string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.watches, identity)
	h.saveLocked()
}

// watchStatus 是 status 动词里的一行关注报告。
type watchStatus struct {
	TreeWatch
	// Head 是当前分支头。
	Head string
	// Changed 表示当前头与 Since 不同（即锚定之后有推进）。
	Changed bool
	// Error 非空说明这棵树查不了（被删、无权限等）。
	Error string
}

// report 把关注标记算成可执行的现状。
//
// 只报告「变了没有」和「当前头是什么」：具体差量由调用方用
// since <tree> <since> 取。不在这里塞正文——status 是概览，
// 把内容铺进状态输出会让它变成一次没有上限的读操作。
func (s *Server) watchReport(c Caller) []watchStatus {
	watches := s.treeWatch.List(c.Identity)
	out := make([]watchStatus, 0, len(watches))
	for _, w := range watches {
		st := watchStatus{TreeWatch: w}
		if !s.allowed(c, w.Project, w.Branch, configBitRead) {
			st.Error = "no read permission (watch kept until cleared)"
			out = append(out, st)
			continue
		}
		repo, err := s.projects.Get(w.Project)
		if err != nil {
			st.Error = err.Error()
			out = append(out, st)
			continue
		}
		head, err := repo.Resolve(w.Branch)
		if err != nil || head == "" {
			st.Error = "branch not found"
			out = append(out, st)
			continue
		}
		st.Head = head
		st.Changed = w.Since != "" && head != w.Since
		out = append(out, st)
	}
	return out
}

// formatWatchReport 把关注现状渲染成 status 里的若干行。
func formatWatchReport(rows []watchStatus) []string {
	if len(rows) == 0 {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		where := r.Project + "@" + r.Branch
		if r.Path != "" {
			where += "/" + r.Path
		}
		switch {
		case r.Error != "":
			out = append(out, fmt.Sprintf("%s  [%s]", where, r.Error))
		case r.Head == "":
			out = append(out, where+"  (empty branch)")
		case r.Changed:
			// 明确给出行之有效的下一步：拿 since 取差量。
			out = append(out, fmt.Sprintf("%s  MOVED %s -> %s  (use: since %s %s)",
				where, shortSHA(r.Since), shortSHA(r.Head), where, r.Since))
		default:
			out = append(out, fmt.Sprintf("%s  unchanged (%s)", where, shortSHA(r.Head)))
		}
	}
	return out
}
