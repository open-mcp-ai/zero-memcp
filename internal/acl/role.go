// Package acl 里还有一份**收窄档案**（role）：命名的一组「只能更窄」的
// 权限边界，供调用方在请求里声明，用于给子 agent 进一步限制。
//
// # 与 Rule（授权）的区别，一句话
//
//	Rule = 「这格开没开」  —— 相对身份而言，回答「你能不能碰这条分支」
//	Role = 「再收窄到多小」 —— 相对一次调用而言，回答「这一次只让它看哪块」
//
// # 为什么需要它
//
// 多 agent 共用一个身份是常态：几个子 agent 共用同一张 token（同一个
// claims.name），却各自只该看到自己那一小块——比如贴吧分身只该看
// record/贴吧，群聊分身只该看 record/群聊。给每个子 agent 发一张 token
// 会带来分发问题；而让调用方在请求里**自报角色**如果会放大权限，那就是
// 一个提权漏洞。
//
// 所以 role 的定位被钉死成两件事：
//
//  1. **只能收窄**：role 的每一条都必须在 token 天花板（挂载表 + cap）之内，
//     定义时就校验；调用时再与天花板取交集。于是「被骗着自报 role=admin」
//     拿不到任何凭证本来没有的东西。
//  2. **绑定才可用**：role 必须 bind 给某个身份才能被它使用。这不是安全
//     必需（收窄不会提权），而是正确性与审计必需——否则一个拼错的 role
//     名会静默变成「没有额外限制」，调用方以为收窄了，其实没有。
package acl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// RoleEntry 是 role 里的一条边界：某条分支（或分支内某棵子树）给到某个权限。
type RoleEntry struct {
	Project string `json:"project"`
	// Branch 为空表示该项目所有分支。
	Branch string `json:"branch,omitempty"`
	// Path 是分支内的子树前缀，空表示整条分支。与挂载表的 Path 同义。
	Path string `json:"path,omitempty"`
	// Perm 是这条允许的权限字母（rwx 子集）。role 里没写到的格子一律拒绝。
	Perm string `json:"perm"`
}

// Key 是这条边界的唯一标识（project + branch + path）。
func (e RoleEntry) Key() string {
	b := strings.TrimSpace(e.Branch)
	if b == "" {
		b = "*"
	}
	return strings.Join([]string{e.Project, b, strings.Trim(e.Path, "/")}, "\x00")
}

// Role 是一个命名收窄档案。
type Role struct {
	Name string `json:"name"`
	// Entries 是允许的边界。空表示「什么都不允许」——空 role 不是「不限制」，
	// 因为一个叫「只读某分支」却什么都不允许的档案，比一个悄悄不生效的
	// 档案安全得多（失败要出声）。
	Entries []RoleEntry `json:"entries"`
	// BoundTo 是允许使用这个 role 的凭证身份（claims.name）。
	BoundTo []string `json:"bound_to,omitempty"`
	// Note 给人看：半年后没人记得这条为什么这么窄。
	Note string `json:"note,omitempty"`
	// UpdatedAt 是最后一次改动时间（unix 秒）。
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// Copy 返回一份深拷贝，避免调用方改到 store 里的条目。
func (r Role) Copy() Role {
	out := r
	out.Entries = append([]RoleEntry(nil), r.Entries...)
	out.BoundTo = append([]string(nil), r.BoundTo...)
	return out
}

// Allows 判断这个 role 是否允许 (project, branch, path) 的 perm。
//
// 匹配顺序：精确 > 通配；path 前缀匹配（子树语义）。**没写到的就是不允许**。
func (r Role) Allows(project, branch, path string, perm uint8) bool {
	bit := permLetter(perm)
	if bit == "" {
		return false
	}
	// path 为空表示**分支级**判定：「这条 role 在这个分支上是否给了这个位」。
	//
	// 必须区分这两种语义，否则子树型 role（p@br:record/forum）在分支级
	// 判定里会永远匹配不上——空前缀不等于「任意前缀」。实测踩过：
	// 天花板校验用空路径调用，于是「只允许 record/forum 的只读 role」
	// 把它自己范围内的读也一起拒了。
	branchLevel := strings.TrimSpace(path) == ""
	path = strings.Trim(path, "/")
	for _, e := range r.Entries {
		if e.Project != project && e.Project != "*" {
			continue
		}
		if e.Branch != "" && e.Branch != "*" && e.Branch != branch {
			continue
		}
		if e.Path != "" && !branchLevel {
			p := strings.Trim(e.Path, "/")
			if path != p && !strings.HasPrefix(path, p+"/") {
				continue
			}
		}
		if !strings.Contains(e.Perm, bit) {
			// 这条边界覆盖了这个位置但不给这个位：直接拒绝，
			// 不再看后面更宽的条目——否则一个宽的条目会把明确的
			// 「只读」重新变成「可写」。
			return false
		}
		return true
	}
	return false
}

// permLetter 把权限位映射成字母。与 server 包的 capBit 同构，
// 但这里不能反向依赖 server（acl 是被 server 依赖的底层包）。
func permLetter(perm uint8) string {
	switch perm {
	case 4:
		return "r"
	case 2:
		return "w"
	case 1:
		return "x"
	}
	return ""
}

// RoleStore 是 role 表：内存快照 + 落盘，读多写极少。
type RoleStore struct {
	path  string
	state atomic.Pointer[roleState]
	wmu   chan struct{}
}

type roleState struct {
	roles map[string]Role
	order []string
}

// OpenRoles 打开（或创建）role 表。
func OpenRoles(path string) (*RoleStore, error) {
	st := &RoleStore{path: path, wmu: make(chan struct{}, 1)}
	st.wmu <- struct{}{}
	rs := &roleState{roles: map[string]Role{}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// 第一次运行：空表。
	case err != nil:
		return nil, fmt.Errorf("read roles %s: %w", path, err)
	default:
		var file struct {
			Roles []Role `json:"roles"`
		}
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &file); err != nil {
				return nil, fmt.Errorf("parse roles %s: %w", path, err)
			}
		}
		for _, r := range file.Roles {
			if r.Name == "" {
				continue
			}
			if _, dup := rs.roles[r.Name]; !dup {
				rs.order = append(rs.order, r.Name)
			}
			rs.roles[r.Name] = r.Copy()
		}
	}
	st.state.Store(rs)
	return st, nil
}

// Lookup 取一个 role。第二个返回值是「存不存在」。
func (st *RoleStore) Lookup(name string) (Role, bool) {
	if st == nil {
		return Role{}, false
	}
	rs := st.state.Load()
	if rs == nil {
		return Role{}, false
	}
	r, ok := rs.roles[name]
	if !ok {
		return Role{}, false
	}
	return r.Copy(), true
}

// Roles 返回全部 role（按名排序），供审计。
func (st *RoleStore) Roles() []Role {
	if st == nil {
		return nil
	}
	rs := st.state.Load()
	if rs == nil {
		return nil
	}
	out := make([]Role, 0, len(rs.order))
	for _, name := range rs.order {
		if r, ok := rs.roles[name]; ok {
			out = append(out, r.Copy())
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Put 新增或替换一个 role。
func (st *RoleStore) Put(r Role) error {
	if st == nil {
		return errors.New("this instance has no role store")
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("role name is required")
	}
	if err := ValidateRoleName(r.Name); err != nil {
		return err
	}
	for _, e := range r.Entries {
		if strings.TrimSpace(e.Project) == "" {
			return fmt.Errorf("role %q: entry needs a project", r.Name)
		}
		if e.Perm == "" {
			return fmt.Errorf("role %q: entry %s@%s needs a perm (r/w/x)", r.Name, e.Project, e.Branch)
		}
		for _, ch := range e.Perm {
			if !strings.ContainsRune("rwx", ch) {
				return fmt.Errorf("role %q: invalid perm %q (allowed letters: rwx)", r.Name, e.Perm)
			}
		}
	}
	<-st.wmu
	defer func() { st.wmu <- struct{}{} }()
	rs := st.state.Load()
	next := &roleState{roles: map[string]Role{}, order: append([]string(nil), rs.order...)}
	for k, v := range rs.roles {
		next.roles[k] = v
	}
	if _, existed := next.roles[r.Name]; !existed {
		next.order = append(next.order, r.Name)
	}
	next.roles[r.Name] = r.Copy()
	if err := st.persist(next); err != nil {
		return err
	}
	st.state.Store(next)
	return nil
}

// Bind 把一个 role 绑给若干身份（覆盖式：给的就是完整清单）。
func (st *RoleStore) Bind(name string, identities []string) (Role, error) {
	if st == nil {
		return Role{}, errors.New("this instance has no role store")
	}
	<-st.wmu
	defer func() { st.wmu <- struct{}{} }()
	rs := st.state.Load()
	cur, ok := rs.roles[name]
	if !ok {
		return Role{}, fmt.Errorf("no role named %q", name)
	}
	next := &roleState{roles: map[string]Role{}, order: append([]string(nil), rs.order...)}
	for k, v := range rs.roles {
		next.roles[k] = v
	}
	cur.BoundTo = append([]string(nil), identities...)
	sort.Strings(cur.BoundTo)
	next.roles[name] = cur
	if err := st.persist(next); err != nil {
		return Role{}, err
	}
	st.state.Store(next)
	return cur.Copy(), nil
}

// Delete 删除一个 role。
func (st *RoleStore) Delete(name string) error {
	if st == nil {
		return errors.New("this instance has no role store")
	}
	<-st.wmu
	defer func() { st.wmu <- struct{}{} }()
	rs := st.state.Load()
	if _, ok := rs.roles[name]; !ok {
		return fmt.Errorf("no role named %q", name)
	}
	next := &roleState{roles: map[string]Role{}, order: nil}
	for k, v := range rs.roles {
		if k == name {
			continue
		}
		next.roles[k] = v
	}
	for _, n := range rs.order {
		if n != name {
			next.order = append(next.order, n)
		}
	}
	if err := st.persist(next); err != nil {
		return err
	}
	st.state.Store(next)
	return nil
}

func (st *RoleStore) persist(rs *roleState) error {
	file := struct {
		Roles []Role `json:"roles"`
	}{}
	for _, name := range rs.order {
		if r, ok := rs.roles[name]; ok {
			file.Roles = append(file.Roles, r)
		}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(st.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".roles-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
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
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, st.path)
}

// ValidateRoleName 限制 role 名成安全标识：它会被写进 JSON、
// 出现在错误信息里，简单的字符集能让日志可读也可 grep。
func ValidateRoleName(name string) error {
	if len(name) > 64 {
		return fmt.Errorf("role name too long (max 64): %q", name)
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return fmt.Errorf("role name %q contains %q (allowed: letters, digits, - _ .)", name, c)
		}
	}
	return nil
}

// RoleEntryAllowsAddr 是给 server 层用的便捷判定：给定完整地址
// （project/branch/path 三者），role 是否放行 perm。
func (r Role) RoleEntryAllowsAddr(project, branch, path string, perm uint8) bool {
	return r.Allows(project, branch, path, perm)
}

// ParseRoleSpec 解析 `proj@branch:subtree` 形状的边界写法。
//
// 三种写法都接受（与地址语法一致，不另造一套）：
//
//	proj                 整个项目
//	proj@branch          那条分支
//	proj@branch:sub      那条分支的子树
func ParseRoleSpec(spec string) (RoleEntry, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return RoleEntry{}, errors.New("empty role scope")
	}
	var e RoleEntry
	proj, rest, hasBranch := strings.Cut(spec, "@")
	e.Project = proj
	if hasBranch {
		branch, path, hasPath := strings.Cut(rest, ":")
		e.Branch = branch
		if hasPath {
			e.Path = strings.Trim(path, "/")
		}
	}
	if e.Project == "" {
		return RoleEntry{}, fmt.Errorf("role scope %q has no project", spec)
	}
	if hasBranch && e.Branch == "" {
		return RoleEntry{}, fmt.Errorf("role scope %q has an empty branch", spec)
	}
	return e, nil
}
