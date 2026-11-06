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
	// 分支锁：锁住的分支只留「不受限的 root」。
	//
	// 对读写一视同仁：锁的目的是「停下来让 root 处理」，
	// 一个还能继续读的分支不算停下来。
	//
	// 这里必须用 rootUnrestricted 而不是 c.Root：
	// root 给分身签的派生凭证，Sub 仍然是 root（是 root 签的），
	// Root 因此为真——只看 Root 就等于所有分身都能绕过锁，
	// 而锁恰恰是为了拦住它们。判定标准是「这张凭证有没有被收窄」。
	if !s.rootUnrestricted(c) && s.branchLocked(project, branch) {
		return false
	}
	// 凭证层：项目必须在这张凭证的挂载表里
	if !c.Mount.Visible(project, branch) {
		return false
	}

	// 凭证层：能力上限。
	//
	// 两处来源取交集：凭证级 cap（整张凭证的天花板）与
	// 该挂载点的 cap（这一个仓库的天花板）。这样一张凭证就能表达
	// 「写自己的仓库、别处只读」，不必为此发两张。
	effective := c.Cap
	if pc := c.Mount.CapOf(project, branch); pc != "" {
		effective = intersectCap(c.Cap, pc)
	}

	// cap 里被按住的那几位，要靠临时授权打开。
	//
	// 判定顺序是「先问这个位在不在上限里，再问它此刻有没有被放行」，
	// 而不是「有授权就给这个位」——后者会让授权变成绕开 cap 的通道。
	// 所以 Gated 必须同时是 cap 的子集（签发时已校验），
	// 而授权又必须是 Gated 的子集（管理台已校验）：
	// 三层各自收窄，谁也不能往上顶。
	bit := capBit(perm)
	if !hasCap(effective, bit) {
		return false
	}
	if gatedHas(c.Gated, bit) && !s.grantAllows(c, project, branch, bit) {
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
	return true
}

// projectWritableFor 报告凭证是否至少能写该项目的一条已存在分支。
func (s *Server) projectWritableFor(c Caller, project string) bool {
	repo, err := s.projects.Get(project)
	if err != nil {
		branch := s.defaultBranch
		return s.allowed(c, project, branch, configBitWrite)
	}
	refs, err := repo.Refs("refs/heads/")
	if err != nil {
		return false
	}
	if len(refs) == 0 {
		branch := s.defaultBranch
		return s.allowed(c, project, branch, configBitWrite)
	}
	for _, ref := range refs {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if s.allowed(c, project, branch, configBitWrite) {
			return true
		}
	}
	return false
}

// requireRootAccess 把「管理身份」与「本次凭证权限」分开检查。
// root 是 root-only 管理动词的必要条件，但 token 的 cap/mount 仍是上限，
// 不能因为主体是 root 就绕过它们。
func (s *Server) requireRootAccess(c Caller, project, branch string, perm uint8) error {
	if err := s.requireRoot(c); err != nil {
		return err
	}
	if !s.allowed(c, project, branch, perm) {
		return fmt.Errorf("%s", s.denyReason(c, project, branch, perm))
	}
	return nil
}

// requireRootCap 用于全局管理资源（如配置文件），它们没有项目挂载路径，
// 但仍受签发时的凭证能力上限约束。
func (s *Server) requireRootCap(c Caller, bit rune) error {
	if err := s.requireRoot(c); err != nil {
		return err
	}
	if !hasCap(c.Cap, bit) {
		return fmt.Errorf("credential capability denied (missing %c permission)", bit)
	}
	return nil
}

// requireRootGlobal 用于没有项目路径的全局管理资源（配置、用户、密钥库、reload）。
//
// 与 requireRootCap 的差别就是多一条「这张凭证没被限制过」：
// 一张只读某个项目某分支的凭证，主体仍然是 root 用户，只查 Root+cap
// 就会让它列密钥名、读全局配置——而它被签发时那条限制的本意，
// 就是「这张纸只管那一摊」。项目内的管理动词走 requireRootAccess，
// 那里挂载本来就是判定的一部分，不受这条影响。
func (s *Server) requireRootGlobal(c Caller, bit rune) error {
	if err := s.requireRootCap(c, bit); err != nil {
		return err
	}
	if !c.Mount.Empty() {
		return fmt.Errorf("this is a restricted credential (mounts are limited); global administration verbs require an unrestricted credential")
	}
	return nil
}

// denyReason 给出拒绝理由。
func (s *Server) denyReason(c Caller, project, branch string, perm uint8) string {
	// 锁的理由必须排在第一位。
	//
	// 否则调用者会看到「不在这张凭证的可见范围内」——而它明明挂载着
	// 这个项目，于是去怀疑配置、怀疑 token，查很久才想到锁。
	if !s.rootUnrestricted(c) && s.branchLocked(project, branch) {
		return fmt.Sprintf("%s@%s is locked by root; read and write access for non-root credentials is suspended", project, branch)
	}
	if !c.Mount.Visible(project, branch) {
		return fmt.Sprintf("project %q is outside this credential's visible scope", project)
	}
	effective := c.Cap
	if pc := c.Mount.CapOf(project, branch); pc != "" {
		effective = intersectCap(c.Cap, pc)
	}
	bit := capBit(perm)
	if !hasCap(effective, bit) {
		return fmt.Sprintf("credential or mount capability denied (missing %s permission)", string(bit))
	}
	if gatedHas(c.Gated, bit) && !s.grantAllows(c, project, branch, bit) {
		return fmt.Sprintf("%s access to %s@%s is not active (this credential holds that bit until the admin panel grants it)", string(bit), project, branch)
	}
	if s.cfgIndex() == nil {
		return "access denied"
	}
	// 这一层只剩配置（TOML 的 mode/owner/group）。
	// 早先这里还有一段「被角色挡住」的判定，逐请求切换身份的
	// 机制删掉之后它就永远不可达了——留着只会让人以为还有角色。
	return config.Explain(s.cfgIndex(), c.User, project, perm)
}

// rootUnrestricted 报告这是否是一张没有被收窄过的 root 凭证。
//
// 「root」是用户的属性，「这张纸有没有被限制」是凭证的属性。
// 派生出去的凭证（给分身用的那些）主体仍是 root，但它被挂载表
// 或被挂载表收窄过——那它就不是全权凭证，管理动作与分支锁都不该让它过。
func (s *Server) rootUnrestricted(c Caller) bool {
	return c.Root && c.Mount.Empty() &&
		hasCap(c.Cap, 'r') && hasCap(c.Cap, 'w') && hasCap(c.Cap, 'x')
}

// branchLocked 报告某条分支是否被 lock 锁住。
//
// 仓库打开失败时返回 false：锁是「额外的拒绝理由」，
// 不是「拒绝的唯一理由」。仓库都打不开时，后面的判定自然会拒绝，
// 这里再报一次错只会掩盖真正的原因。
func (s *Server) branchLocked(project, branch string) bool {
	repo, err := s.projects.Get(project)
	if err != nil {
		return false
	}
	locked, _ := repo.BranchLocked(branch)
	return locked
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

// capNothing 是「空集」的哨兵：两个非空能力集取交集可能为空，
// 而空串在整套代码里表示「不额外限制」。同一个值承担两种含义，
// 结果就是「不相交的交集」反而得到最宽的结果——
// 只读凭证配上只写挂载点，反而拿到写权限（已实测复现）。
//
// 哨兵不含任何 rwx 字符，所以 hasCap 对每一位都判假，
// 而它非空，又不会退化成「不限制」。
const capNothing = "-"

// intersectCap 取两个能力串的交集。
//
// 空串表示「不额外限制」，等价于 rwx。所以 intersect("", "r") = "r"，
// 而 intersect("rw", "r") = "r"——凭证只能收窄，这个函数是它的实现。
//
// 两个非空集完全不相交时返回 capNothing，不是空串。
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
	if len(out) == 0 {
		return capNothing
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
	node := name
	if s.cfg.Endpoint != "" {
		node = s.cfg.Endpoint
	}
	return Caller{User: name, Identity: node, Root: true, Cap: "rwx"}
}

// cfg 返回当前的配置索引，可能为 nil（无配置文件时）。
//
// 所有读配置的地方都走它，而不是直接碰字段：
// reload 会原子替换指针，直接读字段会绕过 atomic 的可见性保证。
func (s *Server) cfgIndex() *config.Lookup {
	return s.cfgLookup.Load()
}

// grantAllows 报告某张凭证在 (project, branch) 上是否被临时授予了某个能力位。
//
// 每次判定都读盘，不缓存——与吊销名单同一个理由：权限回收必须是即时的。
// 缓存会让「刚收回的权限还能用一会儿」，而那一会儿足够写进不该写的东西。
func (s *Server) grantAllows(c Caller, project, branch string, bit rune) bool {
	if s.grants == nil || c.JTI == "" {
		return false
	}
	return s.grants.Allowed(c.JTI, project, branch, bit)
}

// gatedHas 报告某个能力位是否在这张凭证被按住的集合里。
func gatedHas(gated string, bit rune) bool {
	return strings.ContainsRune(gated, bit)
}
