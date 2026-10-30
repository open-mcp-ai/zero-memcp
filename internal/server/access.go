package server

import (
	"fmt"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// allowed 判定这个调用者对某个项目能否做 perm。
//
// 两层，取交集，任一层拒绝即拒绝：
//
//  1. 凭证层（cap + mount）：这张 token 最远能干到哪
//  2. 配置层（TOML mode/owner/group）：这个人一般被允许做什么
//
// 凭证只能收窄，不能放宽——所以「改了配置不用重发 token」依然成立，
// 而 root 又能为一次具体任务签一张更窄的凭证。
func (s *Server) allowed(c Caller, project, branch string, perm uint8) bool {
	// 凭证层：项目必须在这张凭证的挂载表里
	if !c.Mount.Visible(project, branch) {
		return false
	}
	// 凭证层：能力上限。
	//
	// 两处来源取交集：凭证级 cap（整张凭证的天花板）与
	// 该挂载点的 cap（这一个仓库的天花板）。这样一张凭证就能表达
	// 「写自己的仓库、只读 daily」，不必为此发两张。
	effective := c.Cap
	if pc := c.Mount.CapOf(project, branch); pc != "" {
		effective = intersectCap(c.Cap, pc)
	}
	if !hasCap(effective, capBit(perm)) {
		return false
	}
	// 配置层
	if s.cfgIndex() == nil {
		return true
	}
	// 磁盘上存在但配置里没声明的项目：root 直接放行。
	//
	// 这条规则来自「放进去就有」：往 <data>/repos/ 里扔一个裸仓库，
	// 它就该能读能写，不需要再回服务器改配置重启。
	// 非 root 仍按配置走——自动发现的项目默认不给别人开门。
	if !s.cfgIndex().Declared(project) {
		return c.Root
	}
	if !config.Check(s.cfgIndex(), c.User, project, perm) {
		return false
	}
	// 配置层：角色也要过一遍。
	//
	// 为什么必须有这一步：凭证的 sub 是「这张 token 是谁签的」，
	// 角色是「这次请求以谁的名义做」。两者不是一回事——sub 是 root，
	// 而 X-Zero-Role: chat 要的是一份受限视角。只看 sub 的话，
	// TOML 里 [[access]] user="chat" perm="r" 这行永远不会被查到，
	// chat 于是拿着 root 的配置层权限写进了只该读的仓库。
	//
	// 取交集而不是替换：凭证只能收窄、不能放宽，这条不变。
	// 角色名在配置里没声明为 [[user]] 时跳过——那是纯凭证层的角色，
	// 权限已经由挂载表和 cap 定死，不为它凭空造一个用户。
	if s.roleNarrows(c) && !config.Check(s.cfgIndex(), c.Role, project, perm) {
		return false
	}
	return true
}

// roleNarrows 报告本次请求要不要额外拿角色名去配置层判一遍。
//
// 三个条件缺一不可：请求选了角色、角色名与凭证主体不同（否则判的是
// 同一个人，白跑一次）、而且这个名字在 TOML 里真的被声明成了 [[user]]
// （没声明的角色只是凭证里的能力包，配置层没有它的档案，不能凭空造）。
func (s *Server) roleNarrows(c Caller) bool {
	if c.Role == "" || c.Role == c.User {
		return false
	}
	_, ok := s.cfgIndex().User(c.Role)
	return ok
}

// denyReason 给出拒绝理由。
func (s *Server) denyReason(c Caller, project, branch string, perm uint8) string {
	if !c.Mount.Visible(project, branch) {
		return fmt.Sprintf("项目 %q 不在这张凭证的可见范围内", project)
	}
	effective := c.Cap
	if pc := c.Mount.CapOf(project, branch); pc != "" {
		effective = intersectCap(c.Cap, pc)
	}
	bit := capBit(perm)
	if !hasCap(effective, bit) {
		return fmt.Sprintf("凭证/挂载点能力受限（缺少 %s 权限）", string(bit))
	}
	if s.cfgIndex() == nil {
		return "无权访问"
	}
	if !config.Check(s.cfgIndex(), c.User, project, perm) {
		return config.Explain(s.cfgIndex(), c.User, project, perm)
	}
	// 被角色挡住时要说清是角色挡的，否则排查时只会看到
	// 「root 为什么被拒」这种没法解释的结论。
	if s.roleNarrows(c) && !config.Check(s.cfgIndex(), c.Role, project, perm) {
		return fmt.Sprintf("角色 %q：%s", c.Role, config.Explain(s.cfgIndex(), c.Role, project, perm))
	}
	return config.Explain(s.cfgIndex(), c.User, project, perm)
}

// readableBlock 判断调用者能否读某个块。
func (s *Server) readableBlock(c Caller, b memory.Block) bool {
	return s.allowed(c, b.Project, b.Branch, configBitRead)
}

// executableBlock 判断调用者能否把某个块当规则执行（注入系统提示）。
//
// 注入是 x 位：一张没有 x 的凭证拿不到任何规则块，
// 它在自己的世界里就没有宪法——这正是「不含 x 的凭证不注入」的意思。
func (s *Server) executableBlock(c Caller, b memory.Block) bool {
	return s.allowed(c, b.Project, b.Branch, configBitExecute)
}

const (
	configBitRead    uint8 = 4
	configBitWrite   uint8 = 2
	configBitExecute uint8 = 1
)

// capBit 把权限位映射成能力字符。
//
//	r 读内容 · w 写内容 · x 规则注入
func capBit(perm uint8) rune {
	switch perm {
	case configBitRead:
		return 'r'
	case configBitWrite:
		return 'w'
	case configBitExecute:
		return 'x'
	}
	return 0
}

// intersectCap 取两个能力串的交集。
//
// 空串表示「不额外限制」，等价于 rwx。所以 intersect("", "r") = "r"，
// 而 intersect("rw", "r") = "r"——凭证只能收窄，这个函数是它的实现。
func intersectCap(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	var out []rune
	for _, ch := range "rwx" {
		if strings.ContainsRune(a, ch) && strings.ContainsRune(b, ch) {
			out = append(out, ch)
		}
	}
	return string(out)
}

// rootCaller 返回一个配置中真实 root 用户的 Caller，没有则退回 "root"。
func (s *Server) rootCaller() Caller {
	name := "root"
	if s.cfgIndex() != nil {
		for _, u := range s.cfgIndex().Users() {
			if u.Root {
				name = u.Name
				break
			}
		}
	}
	return Caller{User: name, Root: true, Cap: "rwx"}
}

// cfg 返回当前的配置索引，可能为 nil（无配置文件时）。
//
// 所有读配置的地方都走它，而不是直接碰字段：
// reload 会原子替换指针，直接读字段会绕过 atomic 的可见性保证。
func (s *Server) cfgIndex() *config.Lookup {
	return s.cfgLookup.Load()
}
