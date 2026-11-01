package server

import (
	"encoding/json"
	"strings"
	"sync"
)

// TreeWatch 记录某个调用者关注的树路径（前缀匹配）。
//
// 为什么按前缀：树路径天然是层级（例如 "ctx/team" 涵盖 "ctx/team/2026-10-31/..."）。
// 只要子树或该路径下的条目变化，就应提醒，不需要针对每个末端文件逐一声明。
type TreeWatch struct {
	Project string `json:"project"`
	Branch  string `json:"branch"`
	Path    string `json:"path"` // 为空表示关注整个分支
}

// TreeEvent 是分支头推进后广播给客户端的树级变更事件。
//
// Previous 是这次推进之前的分支头：客户端拿它当 since 的起点，
// 就能立刻取回差量。没有它，客户端只知道「变了」，还得自己猜起点。
type TreeEvent struct {
	Project  string   `json:"project"`
	Branch   string   `json:"branch"`
	Previous string   `json:"previous"`
	Commit   string   `json:"commit"`
	Tree     string   `json:"tree"`
	Paths    []string `json:"paths,omitempty"`
	// Truncated 表示 Paths 只是前若干条，真实变化更多。
	Truncated bool `json:"truncated,omitempty"`
}

// maxEventPaths 限制单条事件里携带的路径数。
//
// 路径列表只是「哪棵子树动过」的提示，不是内容本身；一次写入几千个
// 文件时把全部路径塞进 SSE 帧，会把通知变成一次小型数据传输。
const maxEventPaths = 200

// TreeWatchHub 管理客户端在当前连接生命周期内登记的树关注点。
type TreeWatchHub struct {
	mu      sync.RWMutex
	watches map[string][]TreeWatch // identity -> watches
}

func newTreeWatchHub() *TreeWatchHub {
	return &TreeWatchHub{watches: make(map[string][]TreeWatch)}
}

// Add 为某个身份增加一个关注路径。
func (h *TreeWatchHub) Add(identity string, w TreeWatch) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.watches[identity]
	for _, cur := range list {
		if cur.Project == w.Project && cur.Branch == w.Branch && cur.Path == w.Path {
			return
		}
	}
	h.watches[identity] = append(list, w)
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

// Clear 清空该身份的关注（连接断开或显式重置）。
func (h *TreeWatchHub) Clear(identity string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.watches, identity)
}

// Matches 判断该事件是否击中了该身份关注的树路径。
func (h *TreeWatchHub) Matches(identity string, ev TreeEvent) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	watches := h.watches[identity]
	if len(watches) == 0 {
		return false
	}
	for _, w := range watches {
		if w.Project != ev.Project || w.Branch != ev.Branch {
			continue
		}
		if w.Path == "" {
			return true
		}
		for _, changed := range ev.Paths {
			if changed == w.Path || strings.HasPrefix(changed, w.Path+"/") {
				return true
			}
		}
		// 路径列表被截断时不能断言「没变」：宁可多提醒一次，
		// 也不要让一次大范围变更在锚定者那里静默消失。
		if ev.Truncated {
			return true
		}
	}
	return false
}

// broadcastTreeEvent 序列化并广播树变更。
func (s *Server) broadcastTreeEvent(ev TreeEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.bcast.Send("tree", string(data))
}

// branchHead 是一个项目分支当前可观察到的提交。
type branchHead struct {
	Project string
	Branch  string
	Commit  string
}

func treeHeadKey(project, branch string) string { return project + "\x00" + branch }

// treeHeadsSnapshot 读出本机全部项目的当前 heads。
func (s *Server) treeHeadsSnapshot() map[string]branchHead {
	out := make(map[string]branchHead)
	projects, err := s.projects.Projects()
	if err != nil {
		return out
	}
	for _, project := range projects {
		repo, err := s.projects.Get(project)
		if err != nil {
			continue
		}
		for _, branch := range repo.Branches() {
			sha, err := repo.Resolve(branch)
			if err != nil || sha == "" {
				continue
			}
			out[treeHeadKey(project, branch)] = branchHead{Project: project, Branch: branch, Commit: sha}
		}
	}
	return out
}

// treeEventsSinceBaseline 把分支头的变化转为树差分事件。
//
// 单一真相源是文件系统里的 refs 与对象；lastCommits 只是上次观察到
// 的水位。服务重启后水位会重建，因此事件可以丢、但内容不会丢——
// 客户端用保存的 commit 调 since 补齐即可。
func (s *Server) treeEventsSinceBaseline() []TreeEvent {
	current := s.treeHeadsSnapshot()
	var events []TreeEvent

	// 先看当前仍存在的分支。
	for key, head := range current {
		oldValue, ok := s.lastCommits.Load(key)
		if !ok {
			continue // 首次发现的分支只是建立基线，不凭空制造内容变更
		}
		oldCommit, _ := oldValue.(string)
		if oldCommit == head.Commit {
			continue
		}
		event, changed := s.makeTreeEvent(head.Project, head.Branch, oldCommit, head.Commit)
		if changed {
			events = append(events, event)
		}
	}

	// 再看被删除/归档的分支：树从「有」变成「不存在」也是变更。
	s.lastCommits.Range(func(key, value any) bool {
		k, ok := key.(string)
		if !ok {
			return true
		}
		if _, exists := current[k]; exists {
			return true
		}
		oldCommit, _ := value.(string)
		project, branch, ok := splitTreeHeadKey(k)
		if !ok || oldCommit == "" {
			return true
		}
		event, changed := s.makeTreeEvent(project, branch, oldCommit, "")
		if changed {
			events = append(events, event)
		}
		return true
	})
	return events
}

func splitTreeHeadKey(key string) (project, branch string, ok bool) {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == 0 {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

func (s *Server) makeTreeEvent(project, branch, oldCommit, newCommit string) (TreeEvent, bool) {
	repo, err := s.projects.Get(project)
	if err != nil {
		return TreeEvent{}, false
	}
	oldTree, _ := repo.TreeAt(oldCommit, "")
	newTree, _ := repo.TreeAt(newCommit, "")
	if oldTree == newTree {
		return TreeEvent{}, false
	}

	ev := TreeEvent{
		Project:  project,
		Branch:   branch,
		Previous: oldCommit,
		Commit:   newCommit,
		Tree:     newTree,
	}
	changes, err := repo.SubtreeDiff(oldCommit, newCommit, "")
	if err != nil {
		// 旧 commit 可能已不可达（例如重置后刚好跑过 gc）。
		// 不伪造差量；用 Truncated 提醒所有该分支锚定者自行全量重扫。
		ev.Truncated = true
		return ev, true
	}
	for _, ch := range changes {
		ev.Paths = append(ev.Paths, ch.Path)
		if len(ev.Paths) > maxEventPaths {
			ev.Paths = ev.Paths[:maxEventPaths]
			ev.Truncated = true
			break
		}
	}
	return ev, true
}

// rememberTreeHeads 用当前文件系统状态重建水位。
func (s *Server) rememberTreeHeads() {
	current := s.treeHeadsSnapshot()
	alive := make(map[string]bool, len(current))
	for key, head := range current {
		alive[key] = true
		s.lastCommits.Store(key, head.Commit)
	}
	s.lastCommits.Range(func(key, _ any) bool {
		if k, ok := key.(string); ok && !alive[k] {
			s.lastCommits.Delete(k)
		}
		return true
	})
}
