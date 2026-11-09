package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// Mount 是一张凭证的挂载表。
//
// 语义是 chroot：这张凭证的世界只有表里这些 (仓库, 分支)，而且它们长成
// 虚拟路径的样子。不在表里的东西**完全不可见**——不是「拒绝了」，
// 是根本不知道存在。
//
// 同一个仓库可以挂多次，每次指向不同分支、不同虚拟路径、不同能力：
//
//	[{"project":"daily","at":"/","branch":"chat","cap":"rw"},
//	 {"project":"daily","at":"/main","branch":"zero","cap":"r"}]
//
// 这就是「同时读写不同分支」的实现——分支不是全局状态，是挂载项的属性。
type Mount struct {
	entries []mountEntry
	// defaultBranch 用来补全没有写 branch 的挂载项。
	defaultBranch string
}

type mountEntry struct {
	project string
	branch  string
	virtual string // 已规范化，"/" 表示根
	cap     string
	// root 是分支内的子路径（树内路径），空表示整棵分支。
	//
	// 为什么需要它：挂载粒度到「项目+分支」时，简的多个分身无法隔离——
	// 贴吧分身和群聊分身都挂 zero@main.record，彼此看得见对方的记录。
	// 加上 root 之后，贴吧分身只挂 main.record:record/贴吧，
	// 群聊分身看不到它，这仍然是「不可见」而不是「被拒绝」。
	//
	// 语义：root 是这个挂载点的**根**。ToVirtual 会把 root 从路径里剥掉
	// （所以 / 在贴吧分身眼里就是 record/贴吧），ToReal 会把它加回去。
	root string
}

// NewMount 从声明构建。defaultBranch 用于补全省略的 branch。
func NewMount(points []auth.MountPoint, defaultBranch string) *Mount {
	if len(points) == 0 {
		return nil
	}
	if defaultBranch == "" {
		defaultBranch = gitstore.DefaultBranch
	}
	m := &Mount{defaultBranch: defaultBranch}
	for _, p := range points {
		proj := strings.Trim(strings.TrimSpace(p.Project), "/")
		if proj == "" {
			continue
		}
		br := strings.TrimSpace(p.Branch)
		if br == "" {
			br = defaultBranch
		}
		m.entries = append(m.entries, mountEntry{
			project: proj,
			branch:  br,
			virtual: normalizeVirtual(p.At),
			cap:     strings.TrimSpace(p.Cap),
			root:    strings.Trim(strings.TrimSpace(p.Path), "/"),
		})
	}
	if len(m.entries) == 0 {
		return nil
	}
	// 最长虚拟路径优先："/shared" 必须排在 "/" 之前
	sort.Slice(m.entries, func(i, j int) bool {
		return len(m.entries[i].virtual) > len(m.entries[j].virtual)
	})
	return m
}

func normalizeVirtual(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "/")
	if v == "" {
		return "/"
	}
	return "/" + v
}

// Empty 报告这张表是不是全命名空间。
func (m *Mount) Empty() bool { return m == nil || len(m.entries) == 0 }

// DefaultBranch 返回补全用的默认分支。
func (m *Mount) DefaultBranch() string {
	if m == nil || m.defaultBranch == "" {
		return gitstore.DefaultBranch
	}
	return m.defaultBranch
}

// BranchesOf 列出这张挂载表里某个项目被授权的分支。
//
// "*" 展开成具体分支由调用方决定（它需要知道分支是否真的存在）；
// 这里原样返回，让判断集中在调用点。
func (m *Mount) BranchesOf(project string) []string {
	if m == nil || m.Empty() {
		return nil
	}
	var out []string
	for _, e := range m.entries {
		if e.project != project {
			continue
		}
		dup := false
		for _, b := range out {
			if b == e.branch {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, e.branch)
		}
	}
	return out
}

// Lookup 找出覆盖 (project, branch) 的挂载项。
//
// 这是「分支级」的粗查：只回答「这个分支对这张凭证是否可见」，
// 不回答「分支里的哪个子树」。需要路径精度时用 LookupPath。
func (m *Mount) Lookup(project, branch string) (mountEntry, bool) {
	if m.Empty() {
		return mountEntry{}, true // 全命名空间
	}
	if branch == "" {
		branch = m.defaultBranch
	}
	// 先找精确匹配，再找通配。
	//
	// 为什么需要通配：根身份要「所有项目的所有分支」，逐个写死分支名
	// 意味着每新建一个分支都得重签凭证。而 "*" 是一个显式的、
	// 写在凭证里的意图，不是「没匹配上就放行」的默认——后者会让
	// 一个写错项目名的请求悄悄落到别处。
	for _, e := range m.entries {
		if e.project == project && e.branch == branch {
			return e, true
		}
	}
	for _, e := range m.entries {
		if e.project == project && e.branch == "*" {
			return e, true
		}
	}
	return mountEntry{}, false
}

// LookupPath 找出覆盖 (project, branch, path) 的挂载项，路径精度。
//
// 与 Lookup 的差别：Lookup 只看分支，LookupPath 还要求 path 落在
// 挂载项的 root 子树内。同一分支挂两次（贴吧 + 群聊）时，只有
// path 落在对应 root 下的那个挂载项才算命中。
//
// 多个挂载项同时覆盖时，取 root 最长的那个——与 virtual 的
// 「最长前缀优先」同一条原则：更具体的那条说了算。
func (m *Mount) LookupPath(project, branch, path string) (mountEntry, bool) {
	if m.Empty() {
		return mountEntry{}, true
	}
	if branch == "" {
		branch = m.defaultBranch
	}
	path = strings.Trim(path, "/")

	var best mountEntry
	var found bool
	for _, e := range m.entries {
		if e.project != project {
			continue
		}
		if e.branch != branch && e.branch != "*" {
			continue
		}
		if e.root != "" && !pathUnderRoot(path, e.root) {
			continue
		}
		if !found || len(e.root) > len(best.root) {
			best, found = e, true
		}
	}
	return best, found
}

// PathVisible 报告 (project, branch, path) 是否对这张凭证可见。
//
// 与 Visible 的差别：Visible 只看分支，PathVisible 还要求 path 落在
// 某个挂载项的 root 子树内。同一分支挂多个子树（贴吧 + 群聊）时，
// 路径决定了哪条挂载项命中，也就决定了该路径是否可见。
func (m *Mount) PathVisible(project, branch, path string) bool {
	_, ok := m.LookupPath(project, branch, path)
	return ok
}

// BranchWide 报告这张凭证对 (project, branch) 是否拥有「整分支」挂载。
//
// 路径受限的挂载（root != ""）返回 false：它能读写自己子树里的内容，
// 但不能做分支级操作（branch/tag/reset/push）——那些操作的作用域
// 是整个分支，超出这张凭证的世界。
func (m *Mount) BranchWide(project, branch string) bool {
	if m.Empty() {
		return true
	}
	if branch == "" {
		branch = m.defaultBranch
	}
	for _, e := range m.entries {
		if e.project != project {
			continue
		}
		if e.branch != branch && e.branch != "*" {
			continue
		}
		if e.root == "" {
			return true
		}
	}
	return false
}

// rootOf 返回覆盖该分支的挂载项里最长的 root，供错误信息说明边界。
func (m *Mount) rootOf(project, branch string) string {
	if m.Empty() {
		return ""
	}
	if branch == "" {
		branch = m.defaultBranch
	}
	best := ""
	for _, e := range m.entries {
		if e.project != project {
			continue
		}
		if e.branch != branch && e.branch != "*" {
			continue
		}
		if len(e.root) > len(best) {
			best = e.root
		}
	}
	return best
}

// pathUnderRoot 判断 path 是否落在 root 子树内（含 root 本身）。
func pathUnderRoot(path, root string) bool {
	if root == "" {
		return true
	}
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+"/")
}

// Visible 报告 (project, branch) 对这张凭证是否可见。
func (m *Mount) Visible(project, branch string) bool {
	_, ok := m.Lookup(project, branch)
	return ok
}

// CapOf 返回该挂载点的能力上限，空表示继承凭证级 cap。
func (m *Mount) CapOf(project, branch string) string {
	e, ok := m.Lookup(project, branch)
	if !ok {
		return ""
	}
	return e.cap
}

// Projects 返回可见的项目名（去重）。
func (m *Mount) Projects() []string {
	if m.Empty() {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range m.entries {
		if !seen[e.project] {
			seen[e.project] = true
			out = append(out, e.project)
		}
	}
	sort.Strings(out)
	return out
}

// ToVirtual 把 (项目, 分支, 树内路径) 翻成虚拟地址。
//
// 不在表里的返回空串——调用方应当据此把它当作不存在，
// 而不是回落到真实地址（回落等于泄漏命名空间）。
func (m *Mount) ToVirtual(project, branch, path string) string {
	if m.Empty() {
		if path == "" {
			return project
		}
		return project + "/" + path
	}
	for _, e := range m.entries {
		if e.project != project {
			continue
		}
		// "*" 是「这个项目的所有分支」。不认它会让根身份
		// 在挂载表里看得见、在虚拟化时又看不见——
		// 表现为 /identity 返回 0 块，而 health 明明数出 9 块。
		if e.branch != branch && e.branch != "*" && !(e.branch == m.defaultBranch && branch == "") {
			continue
		}
		// 路径收窄：root 子树外的内容在这张凭证眼里不存在。
		//
		// 这里返回空串而不是跳过、让下一个更宽的挂载项接手——
		// 跳过会让「贴吧分身看不到群聊记录」失效：群聊那条更宽的
		// 挂载项（如果存在）会把这个路径接走。返回空串是明确的
		// 「不可见」，调用方据此当作不存在。
		p := strings.Trim(path, "/")
		if e.root != "" {
			if !pathUnderRoot(p, e.root) {
				continue
			}
			// root 变成这张凭证的根：剥掉前缀
			if p == e.root {
				p = ""
			} else {
				p = strings.TrimPrefix(p, e.root+"/")
			}
		}
		if e.virtual == "/" {
			return p
		}
		v := strings.TrimPrefix(e.virtual, "/")
		if p == "" {
			return v
		}
		return v + "/" + p
	}
	return ""
}

// ToVirtualAddr 把 "project/path" + branch 翻成虚拟地址。
func (m *Mount) ToVirtualAddr(project, branch, addr string) string {
	path := strings.TrimPrefix(addr, project+"/")
	return m.ToVirtual(project, branch, path)
}

// ToReal 把虚拟地址翻成 (项目, 分支, 树内路径)。
//
// 匹配不到返回错误，绝不猜。猜错的后果是读到另一个仓库里的东西，
// 而且不报错——这是最难查的一类失败。
func (m *Mount) ToReal(virtAddr string) (project, branch, path string, err error) {
	virtAddr = strings.TrimPrefix(strings.TrimSpace(virtAddr), "/")
	if m.Empty() {
		return "", "", "", fmt.Errorf("empty mount table should not reach this path")
	}

	// joinRoot 把虚拟路径还原成树内路径：root 是挂载点的根，
	// 所以凭证眼里的 "贴吧/今天的记录.md" 实际是 "record/贴吧/今天的记录.md"。
	joinRoot := func(e mountEntry, rel string) string {
		rel = strings.Trim(rel, "/")
		if e.root == "" {
			return rel
		}
		if rel == "" {
			return e.root
		}
		return e.root + "/" + rel
	}

	for _, e := range m.entries {
		v := strings.TrimPrefix(e.virtual, "/")
		br := e.branch
		if br == "*" {
			br = m.defaultBranch
		}
		if v == "" {
			// "/" 是根：任何不属于更具体挂载点的路径都归它
			if isUnderOtherMount(virtAddr, m.entries) {
				continue
			}
			return e.project, br, joinRoot(e, virtAddr), nil
		}
		if virtAddr == v {
			return e.project, br, joinRoot(e, ""), nil
		}
		if strings.HasPrefix(virtAddr, v+"/") {
			return e.project, br, joinRoot(e, virtAddr[len(v)+1:]), nil
		}
	}

	return "", "", "", fmt.Errorf("path %q is not in this credential's mount table", virtAddr)
}

// isUnderOtherMount 判断路径是否属于某个更具体的挂载点。
func isUnderOtherMount(addr string, entries []mountEntry) bool {
	for _, e := range entries {
		v := strings.TrimPrefix(e.virtual, "/")
		if v == "" {
			continue
		}
		if addr == v || strings.HasPrefix(addr, v+"/") {
			return true
		}
	}
	return false
}

// Describe 给出一行摘要：这条凭证实际能被看见的范围。
//
// 它服务于 whoami：一个被挂载表收窄过的 root 需要知道自己窄在哪，
// 否则「我有 root 但全局动词被拒」这件事无从解释。
// 格式与凭证签发时的写法一致（project@branch:root），便于对照。
func (m *Mount) Describe() string {
	if m.Empty() {
		return "(none: every namespace is visible)"
	}
	var parts []string
	for _, e := range m.entries {
		s := e.project
		if e.branch != "" && e.branch != "*" {
			s += "@" + e.branch
		} else if e.branch == "*" {
			s += "@*"
		}
		if e.root != "" {
			s += ":" + e.root
		}
		if e.cap != "" {
			s += " (" + e.cap + ")"
		}
		parts = append(parts, s)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
