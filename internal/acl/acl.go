// Package acl 是运行时的分支级授权表。
//
// 为什么不复用配置里的 [[access]]：
//
// 配置是**部署事实**——项目在哪、谁是属主、监听哪个端口。它适合写进文件，
// 因为它需要被审阅、被 diff、被版本记录；改它本来就该重启或 reload。
//
// 而授权是**每天都要改的数据**：今天给某个分身开一条共享分支，明天收回去。
// 把它塞进配置文件，意味着每改一次权限都要重读整份 TOML、重扫全部仓库、
// 还要担心格式和编码（这份配置曾经在 PowerShell 里被写成 GBK，中文注释
// 全烂）。更要命的是它逼着人「改权限 → 重签凭证 → 把新 token 送到每台
// 客户机」——真正麻烦的从来不是签名，是分发。
//
// 所以这份表单独存成 JSON，走 API 热更新，和凭证完全解耦：
// 凭证只回答「你是谁」，这张表回答「你能碰哪条分支」。
package acl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rule 是一条授权。
//
// Branch 留空或 "*" 表示项目里的所有分支；写了具体名就只对那条生效。
// 精确分支优先于通配，因此「项目通配只给 r」+「main.shared 给 rwx」
// 可以同时成立，且不受声明顺序影响。
type Rule struct {
	User    string `json:"user"`
	Project string `json:"project"`
	Branch  string `json:"branch,omitempty"`
	Perm    string `json:"perm"`

	// Admin 表示这条授予实例级管理权（跑 config/reload/users/acl 等动词）。
	//
	// 它是一个独立维度，不是 rwx 的一部分：管理实例和读写记忆是两种权力，
	// 混在同一个字母表里会让「给他读一下这个分支」不小心变成「他能改整个实例」。
	Admin bool `json:"admin,omitempty"`
	// Note 是给人看的备注：半年后没人记得这条为什么开。
	Note string `json:"note,omitempty"`
	// UpdatedAt 是最后一次改动的时间（unix 秒），由 Store 维护。
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// Key 是这条规则的唯一标识。
//
// 「用户 + 项目 + 分支」三者确定一条规则——同一格子里出现两条规则是
// 数据错误，不是优先级问题。写入时按这个键覆盖，不会堆出重复条目。
func (r Rule) Key() string {
	b := strings.TrimSpace(r.Branch)
	if b == "" {
		b = "*"
	}
	return strings.Join([]string{r.User, r.Project, b}, "\x00")
}

// Store 是这份授权表的内存副本 + 落盘。
//
// 读多写极少：每次请求都要判定，而改权限是几天一次的动作。
// 所以内存里保存一份切片，写入时才落盘；读路径不碰文件系统。
type Store struct {
	path string

	mu    sync.RWMutex
	rules []Rule
	// loadErr 记住上次读盘失败的原因，让 status 能回答「为什么一条规则都不生效」。
	// 静默失败在这里代价极高：看起来像「有人把权限删了」。
	loadErr error
}

// Open 绑定一个 JSON 文件并读入。文件不存在时视为空表——不是错误，
// 因为「还没配过授权」和「授权文件坏了」是两件事，不能混在一起。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload 重新读盘。文件不存在 → 空表。
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked()
}

func (s *Store) reloadLocked() error {
	s.loadErr = nil
	if s.path == "" {
		s.rules = nil
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.rules = nil
		return nil
	}
	if err != nil {
		s.loadErr = err
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		s.rules = nil
		return nil
	}
	var doc struct {
		Rules []Rule `json:"rules"`
	}
	// 兼容「文件本身就是一个数组」的写法：手写时更省事。
	if err := json.Unmarshal(raw, &doc); err != nil {
		var bare []Rule
		if err2 := json.Unmarshal(raw, &bare); err2 != nil {
			s.loadErr = err
			return fmt.Errorf("parse %s: %w", s.path, err)
		}
		doc.Rules = bare
	}
	s.rules = normalize(doc.Rules)
	return nil
}

// normalize 去掉空白、丢弃不合法的条目并去重（后者覆盖前者）。
//
// 丢弃而不是报错：这张表是被热编辑的，一条写错的规则不该让整个服务
// 起不来或者全部权限失效。合法性在写入路径（Add/Update）严格校验，
// 读路径选择「能用的先用着」。
func normalize(in []Rule) []Rule {
	out := make([]Rule, 0, len(in))
	seen := map[string]int{}
	for _, r := range in {
		r.User = strings.TrimSpace(r.User)
		r.Project = strings.Trim(strings.TrimSpace(r.Project), "/")
		r.Branch = strings.TrimSpace(r.Branch)
		r.Perm = strings.TrimSpace(r.Perm)
		r.Note = strings.TrimSpace(r.Note)
		if r.User == "" || r.Project == "" {
			continue
		}
		if r.Admin && r.Perm == "" {
			// 纯管理权规则：没有项目权限，只是允许管实例
		} else if !validPerm(r.Perm) {
			continue
		}
		key := r.Key()
		if i, ok := seen[key]; ok {
			out[i] = r
			continue
		}
		seen[key] = len(out)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return out[i].User < out[j].User
		}
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Branch < out[j].Branch
	})
	return out
}

func validPerm(p string) bool {
	if p == "" {
		return false
	}
	for _, c := range p {
		if !strings.ContainsRune("rwx", c) {
			return false
		}
	}
	return true
}

// Rules 返回当前全部规则（副本，调用方随便改）。
func (s *Store) Rules() []Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	return out
}

// LoadErr 返回上次读盘失败的原因（nil 表示正常）。
func (s *Store) LoadErr() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr
}

// Lookup 判定某用户在某项目某分支上被授予的权限字母。
//
// 返回顺序：精确分支优先，其次通配；两者都没有就是「没授权」。
// 注意「没授权」不等于「拒绝」——调用方还要决定没授权时是回落到
// 配置的 mode，还是一律拒绝。这里只回答「这张表说了什么」。
func (s *Store) Lookup(user, project, branch string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return lookup(s.rules, user, project, branch)
}

func lookup(rules []Rule, user, project, branch string) (string, bool) {
	project = strings.Trim(strings.TrimSpace(project), "/")
	branch = strings.TrimSpace(branch)
	wildcard, hasWildcard := "", false
	for _, r := range rules {
		if r.User != user || r.Project != project {
			continue
		}
		if r.Branch == "" || r.Branch == "*" {
			if !hasWildcard {
				wildcard, hasWildcard = r.Perm, true
			}
			continue
		}
		if branch != "" && r.Branch == branch {
			return r.Perm, true
		}
	}
	if hasWildcard {
		return wildcard, true
	}
	return "", false
}

// IsAdmin 报告某身份是否被授予实例级管理权。
//
// 判定只看 user，不看项目：管理权是实例级的，没有「只管这个项目」的
// 半档——真的要分，那是另一个设计（每个项目一个 operator），不是这里。
func (s *Store) IsAdmin(user string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.rules {
		if r.User == user && r.Admin {
			return true
		}
	}
	return false
}

// ErrNotFound 表示要改/删的规则不存在。
var ErrNotFound = errors.New("no such rule")

// Put 插入或覆盖一条规则（按 用户+项目+分支 定位）。
func (s *Store) Put(r Rule) (Rule, error) {
	r.User = strings.TrimSpace(r.User)
	r.Project = strings.Trim(strings.TrimSpace(r.Project), "/")
	r.Branch = strings.TrimSpace(r.Branch)
	r.Perm = strings.TrimSpace(r.Perm)
	r.Note = strings.TrimSpace(r.Note)
	if r.User == "" {
		return Rule{}, errors.New("user is required")
	}
	if r.Project == "" {
		return Rule{}, errors.New("project is required")
	}
	if !r.Admin && !validPerm(r.Perm) {
		return Rule{}, fmt.Errorf("perm %q must be a non-empty subset of rwx", r.Perm)
	}
	if r.Admin && r.Perm != "" && !validPerm(r.Perm) {
		return Rule{}, fmt.Errorf("perm %q must be a subset of rwx", r.Perm)
	}
	if r.Branch == "*" {
		r.Branch = ""
	}
	r.UpdatedAt = time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]Rule, 0, len(s.rules)+1)
	replaced := false
	for _, cur := range s.rules {
		if cur.Key() == r.Key() {
			next = append(next, r)
			replaced = true
			continue
		}
		next = append(next, cur)
	}
	if !replaced {
		next = append(next, r)
	}
	if err := s.commitLocked(next); err != nil {
		return Rule{}, err
	}
	return r, nil
}

// Delete 删除一条规则。分支为空或 "*" 时删除通配那条。
func (s *Store) Delete(user, project, branch string) error {
	user = strings.TrimSpace(user)
	project = strings.Trim(strings.TrimSpace(project), "/")
	branch = strings.TrimSpace(branch)
	if branch == "*" {
		branch = ""
	}
	key := Rule{User: user, Project: project, Branch: branch}.Key()

	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]Rule, 0, len(s.rules))
	found := false
	for _, cur := range s.rules {
		if cur.Key() == key {
			found = true
			continue
		}
		next = append(next, cur)
	}
	if !found {
		return ErrNotFound
	}
	return s.commitLocked(next)
}

// Replace 整体替换（管理台一次保存多条时用）。
func (s *Store) Replace(rules []Rule) error {
	clean := normalize(rules)
	for _, r := range clean {
		if !validPerm(r.Perm) {
			return fmt.Errorf("perm %q must be a subset of rwx", r.Perm)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(clean)
}

// commitLocked 先落盘再换内存。
//
// 顺序不能反：先改内存再写盘的话，写盘失败时内存里就有一份磁盘上没有的
// 授权，重启后权限「莫名其妙消失」，而日志里什么都没有。
func (s *Store) commitLocked(next []Rule) error {
	sorted := normalize(next)
	if s.path != "" {
		if err := writeAtomic(s.path, sorted); err != nil {
			return err
		}
	}
	s.rules = sorted
	s.loadErr = nil
	return nil
}

// writeAtomic 写临时文件再 rename。
//
// 直接覆盖的话，进程在写一半时被杀（hub 被 taskkill 是常态），
// 留下的是半截 JSON——下次启动整个授权表读不出来，全部规则失效。
func writeAtomic(path string, rules []Rule) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if rules == nil {
		rules = []Rule{}
	}
	body, err := json.MarshalIndent(struct {
		Rules []Rule `json:"rules"`
	}{Rules: rules}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".acl-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}