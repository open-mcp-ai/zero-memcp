package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	// state 持有**不可变**的关注表快照。读者 atomic.Load 之后直接用，
	// 不复制、不阻塞；写者造一份新的再换进去（拷贝-修改-交换）。
	//
	// 为什么不用 RWMutex：Add/Clear 要把整张表落盘（json.Marshal + 临时文件 +
	// fsync + rename）。那把这几百微秒到几毫秒的磁盘动作塞进写锁里，
	// 所有读者的 remind/status 都跟着排队。而读是热路径，写是偶尔一次。
	//
	// 用 sync.Mutex 更糟：它没有超时，落盘卡住（网络盘掉线、杀软扫描）
	// 会让后续每个 Add 无限期停在 Lock() 上，且持锁者还在做 I/O ——
	// 这正是 termcp/AGENTS.md:35 记的那 10+ 次死锁的形状。
	state atomic.Pointer[watchState]
	// wmu 是一槽 channel 令牌，只串行化写者（保证拷贝-修改-交换不丢更新），
	// 带超时可退。读者全程不碰它。
	wmu chan struct{}
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

// watchState 是一份不可变快照。
type watchState struct {
	watches map[string][]TreeWatch
}

func (h *TreeWatchHub) snapshot() map[string][]TreeWatch {
	if st := h.state.Load(); st != nil {
		return st.watches
	}
	return nil
}

// beginWrite 取写令牌；返回值必须在写完后调用一次。
func (h *TreeWatchHub) beginWrite() (func(), error) { return tokenAcquire(h.wmu, "watch") }

func newTreeWatchHub() *TreeWatchHub {
	h := &TreeWatchHub{wmu: make(chan struct{}, 1)}
	h.state.Store(&watchState{watches: map[string][]TreeWatch{}})
	return h
}

// newPersistentTreeWatchHub 打开一份落盘的关注表。文件不存在视为空表。
func newPersistentTreeWatchHub(path string) (*TreeWatchHub, error) {
	h := &TreeWatchHub{wmu: make(chan struct{}, 1), path: path}
	h.state.Store(&watchState{watches: map[string][]TreeWatch{}})
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
		// 清洗非法条目：带空白/控制字符的分支名在 Git 里不存在，
		// 它们只可能是历史 bug 留下的幽灵订阅——永远匹配不上，
		// 却让 status 永远报「没变化」。
		//
		// 丢弃必须出声：静默清理会让「我明明 remind 过」变成一次
		// 无从查起的谜。所以逐条报告，并把清洗后的表写回磁盘，
		// 免得下次启动又报一遍。
		dropped := 0
		clean := make(map[string][]TreeWatch, len(doc.Watches))
		for identity, list := range doc.Watches {
			kept := make([]TreeWatch, 0, len(list))
			for _, w := range list {
				if err := validateWatchAddr(w.Project, w.Branch, w.Path); err != nil {
					fmt.Fprintf(os.Stderr, "zero-memcp: watch: dropping invalid entry for %s: %v\n", identity, err)
					dropped++
					continue
				}
				kept = append(kept, w)
			}
			if len(kept) > 0 {
				clean[identity] = kept
			}
		}
		h.state.Store(&watchState{watches: clean})
		if dropped > 0 {
			fmt.Fprintf(os.Stderr, "zero-memcp: watch: %d invalid watch entr(y/ies) dropped; rewritten to %s\n", dropped, path)
			h.saveSnapshot(clean)
		}
	}
	return h, nil
}

// saveSnapshot 把一份关注表写回磁盘。
//
// 写失败不回滚内存：关注丢了只是多读一次树，而把内存改回旧值会让
// 调用方刚做完的 remind 看起来「成功了但没生效」。日志里出声即可。
func (h *TreeWatchHub) saveSnapshot(watches map[string][]TreeWatch) {
	if h.path == "" {
		return
	}
	body, err := json.MarshalIndent(struct {
		Watches map[string][]TreeWatch `json:"watches"`
	}{Watches: watches}, "", "  ")
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

// validateWatchAddr 拒绝不可能存在的关注目标。
//
// 要防的具体故障（3060 hub 实盘见过）：
//
//	watches.json 里出现了一条
//	branch = "main.shared e8103f1809f417e58dc667d977fb92351e73b036"
//
// 成因是调用方把 `since <tree> <commit>` 的写法用在了 `remind` 上：
// `remind proj@main.shared e8103f18...` 里多出来的那个 hash 被
// parseBranchAddr 整段吞进了分支名。它不会报错——只会安静地留下一条
// 永远匹配不上的订阅：status 永远说「没变化」，而分支其实在动。
// 这种「登记成功但永远不生效」比直接报错难查得多。
//
// 判据不是「像不像分支名」，而是「Git 允许不允许」：分支名不能含空白、
// 不能含 ~^:?*[\ 与 ".."，也不能以 / 开头或结尾。带空格的 ref 在 Git
// 里根本不存在，所以这种登记一定是错的。
func validateWatchAddr(project, branch, path string) error {
	if project == "" {
		return fmt.Errorf("watch needs a project")
	}
	if branch == "" {
		return fmt.Errorf("watch needs a branch (use <project>@<branch>[/<path>])")
	}
	// 控制字符（含 NUL）一律拒绝：它们不会出现在合法 ref 里，
	// 而 NUL 尤其危险——它在 C 边界上会截断字符串，让「看起来不同」
	// 的两个名字在底层变成同一个。
	if strings.IndexFunc(branch, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("invalid branch %q in watch: contains control characters", branch)
	}
	if strings.ContainsAny(branch, " \t\r\n") {
		return fmt.Errorf("invalid branch %q in watch: branch names cannot contain whitespace. "+
			"If you meant to set a baseline commit, use `since <tree> <commit>` instead of `remind`", branch)
	}
	if strings.ContainsAny(project, " \t\r\n") {
		return fmt.Errorf("invalid project %q in watch: names cannot contain whitespace", project)
	}
	for _, bad := range []string{"..", "~", "^", ":", "?", "*", "[", "\\"} {
		if strings.Contains(branch, bad) {
			return fmt.Errorf("invalid branch %q in watch: branch names cannot contain %q", branch, bad)
		}
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return fmt.Errorf("invalid branch %q in watch: cannot start or end with /", branch)
	}
	if strings.ContainsAny(path, "\r\n") {
		return fmt.Errorf("invalid path in watch: cannot contain newlines")
	}
	return nil
}

// Add 为某个身份增加或更新一条关注标记（同 project+branch+path 视为同一条）。
func (h *TreeWatchHub) Add(identity string, w TreeWatch) error {
	// 兜底校验放在唯一入口：不管哪个动词写关注，都不可能落下脏记录。
	if err := validateWatchAddr(w.Project, w.Branch, w.Path); err != nil {
		return err
	}
	done, err := h.beginWrite()
	if err != nil {
		return err
	}
	defer done()

	cur := h.snapshot()
	// 拷贝-修改-交换：不改动别人正在读的那份 map。
	next := make(map[string][]TreeWatch, len(cur)+1)
	for k, v := range cur {
		next[k] = v
	}
	list := make([]TreeWatch, len(cur[identity]), len(cur[identity])+1)
	copy(list, cur[identity])
	replaced := false
	for i, c := range list {
		if c.Project == w.Project && c.Branch == w.Branch && c.Path == w.Path {
			// 重新锚定：把基线推到当前头。
			list[i] = w
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, w)
	}
	next[identity] = list

	h.state.Store(&watchState{watches: next})
	h.saveSnapshot(next)
	return nil
}

// List 返回该身份当前关注的树。
func (h *TreeWatchHub) List(identity string) []TreeWatch {
	src := h.snapshot()[identity]
	out := make([]TreeWatch, len(src))
	copy(out, src)
	return out
}

// Clear 清空该身份的全部关注。
func (h *TreeWatchHub) Clear(identity string) error {
	done, err := h.beginWrite()
	if err != nil {
		return err
	}
	defer done()

	cur := h.snapshot()
	next := make(map[string][]TreeWatch, len(cur))
	for k, v := range cur {
		if k == identity {
			continue
		}
		next[k] = v
	}
	h.state.Store(&watchState{watches: next})
	h.saveSnapshot(next)
	return nil
}

// watchStatus 是 status 动词里的一行关注报告。
type watchStatus struct {
	TreeWatch
	// Head 是当前分支头。
	Head string
	// Changed 表示自锚定之后确实有新内容。
	//
	// 判据不再只看头 SHA：工作层是同步主体，一条未提交的写入
	// 也是真实变化，否则「对端推来一条未提交记忆」会被当成没事发生。
	Changed bool
	// WorkLayer 是该分支当前未提交改动的条数。
	//
	// 为什么要报它：工作层同步不移动分支头。只报头 SHA 的话，
	// 一条从未 commit 的分支会被报成「branch not found」——
	// 数据明明在、read 读得到、索引也建了，却在 status 里像不存在。
	WorkLayer int
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
		// 工作层先算：它可能在一个还没 commit 过的分支上。
		// 未提交改动是真实数据，不能因为 refs/heads 里没有这个名字
		// 就当作「分支不存在」。
		st.WorkLayer = repo.Pending(w.Branch)

		head, err := repo.Resolve(w.Branch)
		if err != nil || head == "" {
			if st.WorkLayer > 0 {
				// 分支只存在于工作层：能读、能搜，只是还没封箱。
				// 基线是空的，所以只要工作层有条目就算「有变化」。
				st.Changed = true
				out = append(out, st)
				continue
			}
			st.Error = "branch not found"
			out = append(out, st)
			continue
		}
		st.Head = head
		st.Changed = (w.Since != "" && head != w.Since) || st.WorkLayer > 0
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
		case r.Head == "" && r.WorkLayer > 0:
			// 只在工作层里存在的分支：能读、能搜，还没封箱。
			out = append(out, fmt.Sprintf("%s  work layer only (%d uncommitted; nothing sealed yet)",
				where, r.WorkLayer))
		case r.Head == "":
			out = append(out, where+"  (empty branch)")
		case r.Changed && r.Head != r.Since && r.Since != "":
			// 明确给出行之有效的下一步：拿 since 取差量。
			line := fmt.Sprintf("%s  MOVED %s -> %s  (use: since %s %s)",
				where, shortSHA(r.Since), shortSHA(r.Head), where, r.Since)
			if r.WorkLayer > 0 {
				line += fmt.Sprintf("  + %d uncommitted", r.WorkLayer)
			}
			out = append(out, line)
		case r.WorkLayer > 0:
			// 头没动，但工作层进了新内容——这同样是一次变化，
			// 而且正是工作层同步的工作方式（不依赖 commit）。
			out = append(out, fmt.Sprintf("%s  work layer modified (%d uncommitted; head %s unchanged)",
				where, r.WorkLayer, shortSHA(r.Head)))
		default:
			out = append(out, fmt.Sprintf("%s  unchanged (%s)", where, shortSHA(r.Head)))
		}
	}
	return out
}
