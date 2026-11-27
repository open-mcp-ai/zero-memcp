package server

// aclActive 报告服务端授权表是否可用。
//
// 单独一个方法是为了让「没启用」这件事显式可见：所有权限判定都要先问它，
// 而不是各自去判 s.acl != nil——那种散落的 nil 检查，将来加了第二种
// 存储时必然会漏一处，而漏掉的那处会静默按老规则放行。
func (s *Server) aclActive() bool { return s.acl != nil }

// branchVisible 报告这条凭证能否看到某个 (项目, 分支)。
//
// 与 c.Mount.Visible 的差别：它把服务端授权表也算进来。ACL 命中即视为可见——
// 这正是「加一条共享分支不用重签凭证」能成立的地方：凭证里没这条分支，
// 但服务端表给了它，于是它存在。
//
// 反过来不成立：ACL 没写不等于拒绝。没有命中时退回挂载表，保持对
// 老部署的兼容（它们的权限完全写在凭证里）。
func (s *Server) branchVisible(c Caller, project, branch string) bool {
	// 与 allowed 同一条不变式：凭证天花板是最后一道闸，ACL 只能在里面挑。
	//
	// 曾经这里先看 ACL、再看挂载表，于是「ACL 开出来的分支」在列举里可见、
	// 在 allowed 里也可写——两条路径各自成立，但都越过了 token。
	// 现在统一：挂载表看不见的分支就是看不见，ACL 不会让它出现。
	if !c.Mount.Visible(project, branch) {
		return false
	}
	if s.aclActive() {
		if _, ok := s.acl.Lookup(c.Identity, project, branch); ok {
			return true
		}
	}
	return true
}

// adminAllowed 报告这个身份能否做实例级管理动作。
//
// 两条来源：不受限的 root 凭证（老路子），或 ACL 里显式授予的 admin。
// 后者让「AI 自己通过 MCP 配置实例」成为可能，而不必把一张全权凭证
// 交给它——admin 是一次可撤销的授权，不是一个新身份。
func (s *Server) adminAllowed(c Caller) bool {
	if s.rootUnrestricted(c) {
		return true
	}
	return s.aclActive() && s.acl.IsAdmin(c.Identity)
}

// virtualPath 返回一条块对这张凭证的「可写出口地址」。
//
// 挂载表里的分支用虚拟路径（chroot 语义，看到的是自己的世界）；
// 只被服务端授权表开出来的分支没有虚拟路径，退回真实地址
// project@branch:path——它是空的判据不能是「这棵树不可见」。
//
// 踩过的坑：listings（log/ls）里写成 `if virt == "" { continue }`，
// 于是 ACL 授予的分支在列举里彻底消失，而按地址直接读又能读到。
// 那种「能读但列不出来」的表现会被误判成"权限没生效"。
func (s *Server) virtualPath(c Caller, project, branch, path string) string {
	if v := c.Mount.ToVirtual(project, branch, path); v != "" {
		return v
	}
	// 分支已挂载、但路径落在它的子树 root 之外：这是 chroot 的边界，
	// 一律不可见。绝不能退回真实地址——那样子树分身就能列出
	// 同分支兄弟子树的内容，隔离直接失效（实测会泄漏）。
	if c.Mount.Visible(project, branch) {
		return ""
	}
	if s.branchVisible(c, project, branch) {
		return project + "@" + branch + ":" + path
	}
	return ""
}
