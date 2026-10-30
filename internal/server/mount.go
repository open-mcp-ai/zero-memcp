package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
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
}

// NewMount 从声明构建。defaultBranch 用于补全省略的 branch。
func NewMount(points []auth.MountPoint, defaultBranch string) *Mount {
	if len(points) == 0 {
		return nil
	}
	if defaultBranch == "" {
		defaultBranch = "zero"
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
		return "zero"
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
		if e.virtual == "/" {
			return path
		}
		v := strings.TrimPrefix(e.virtual, "/")
		if path == "" {
			return v
		}
		return v + "/" + path
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
		return "", "", "", fmt.Errorf("空挂载表不该走这条路径")
	}

	for _, e := range m.entries {
		v := strings.TrimPrefix(e.virtual, "/")
		if v == "" {
			// "/" 是根：任何不属于更具体挂载点的路径都归它
			if isUnderOtherMount(virtAddr, m.entries) {
				continue
			}
			br := e.branch
			if br == "*" {
				br = m.defaultBranch
			}
			return e.project, br, virtAddr, nil
		}
		if virtAddr == v {
			br := e.branch
			if br == "*" {
				br = m.defaultBranch
			}
			return e.project, br, "", nil
		}
		if strings.HasPrefix(virtAddr, v+"/") {
			br := e.branch
			if br == "*" {
				br = m.defaultBranch
			}
			return e.project, br, virtAddr[len(v)+1:], nil
		}
	}

	return "", "", "", fmt.Errorf("路径 %q 不在这张凭证的挂载表里", virtAddr)
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
