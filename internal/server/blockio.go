package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// readBlock 读出块的内容。
//
// 这是全服务唯一的内容读取入口。块自己带着 Read 钩子，
// 所以它来自哪个仓库、哪个分支、哪条路径，上层都不需要知道。
func (s *Server) readBlock(b memory.Block) ([]byte, error) {
	if b.Read == nil {
		return nil, errors.New("块没有绑定读取方式")
	}
	return b.Read()
}

// splitAddr 把地址拆成项目名与树内路径。
//
// 项目名由配置声明，可以是 "me" 也可以是 "blackegg/me" 甚至 "a/b/c"。
// 这里按「最长前缀匹配」剥离，所以代码不需要假设层级数。
func (s *Server) splitAddr(addr string) (project, path string, err error) {
	return s.splitAddrPwd(addr, "")
}

// splitAddrPwd 是带默认工作目录的版本。
//
// 给了 pwd 且地址不以任何已声明项目开头时，把地址当成 pwd 下的相对路径。
// 这就是「streamable 里带路径就等于自带一个 pwd」那件事：
// 受限账户在自己的工作区里不需要每次写全路径。
// declaredProjects 返回全部已声明的项目名。
//
// 优先读配置；配置没有（旧模式启动）才扫磁盘。
// 必须以配置为准：一个在配置里声明了、但还没被写过（磁盘上没有 .git）
// 的项目，它的地址必须能被正确解析出来，否则会把全路径误判成相对路径，
// 塞进 pwd 读到了另一个仓库里。
func (s *Server) declaredProjects() []string {
	// 两条来源合并，不是二选一。
	//
	//   磁盘 <data>/repos/<name>.git  → 这个项目存在（放进去就有）
	//   配置 [[project]]              → 这个项目被声明了权限（可能还没内容）
	//
	// 早先写成二选一（有配置就只认配置），代价是：往 repos/ 里放一个
	// 裸仓库，它不会被识别——而「放进去就有」才是这个设计的承诺。
	//
	// 反过来也不能只认磁盘：配置里声明了但还没写过的项目，它的地址
	// 必须能解析出来，否则全路径会被当成相对路径塞进 pwd，
	// 读到了另一个仓库里。
	seen := map[string]bool{}
	var out []string

	if s.cfgIndex() != nil {
		for _, p := range s.cfgIndex().Projects() {
			if !seen[p.Name] {
				seen[p.Name] = true
				out = append(out, p.Name)
			}
		}
	}
	if projs, err := s.projects.Projects(); err == nil {
		for _, p := range projs {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// resolveAddr 把调用者给的地址翻译成 (真实项目, 树内路径)。
//
// 顺序：
//  1. 有挂载表 → 按虚拟路径解析（chroot，表外不可见）
//  2. 无挂载表 → 按已声明项目做最长前缀匹配
//
// 任何一步匹配不到都返回错误，绝不回落到「猜一个」。
func (s *Server) resolveAddr(addr string, c Caller) (project, branch, path string, err error) {
	// 地址里显式写了 @<branch> 时直接用它，绕过挂载表的默认分支。
	//
	// 为什么这条通道必须存在：审计 Agent 要读「别人分支上的脏流」，
	// 清理者要读「冻结批次」，这些都不是它自己的人格分支。没有它，
	// 跨分支协作就只能靠发一堆 token，而 token 是有过期的。
	// 权限不收窄——下面仍然逐条走 allowed()。
	if strings.Contains(addr, "@") {
		proj, br, p, perr := s.parseBranchAddr(addr)
		if perr == nil {
			// 显式分支也必须先过挂载可见性，否则等于绕过 chroot
			if !c.Mount.Visible(proj, br) {
				return "", "", "", fmt.Errorf("项目 %s@%s 不在这张凭证的可见范围内", proj, br)
			}
			return proj, br, p, nil
		}
	}
	if !c.Mount.Empty() {
		return c.Mount.ToReal(addr)
	}
	proj, p, derr := s.splitAddrPwd(addr, "")
	if derr != nil {
		return "", "", "", derr
	}
	br := s.persona
	if br == "" {
		br = "zero"
	}
	return proj, br, p, nil
}

func (s *Server) splitAddrPwd(addr, pwd string) (project, path string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", errors.New("地址不能为空")
	}

	clean := strings.TrimPrefix(addr, "/")
	projects := s.declaredProjects()

	// 1. 最长前缀匹配：优先当成全路径
	if proj, rel, ok := memory.SplitAddrWithProjects(clean, projects); ok && rel != "" {
		return proj, rel, nil
	}

	// 1.5 组织前缀容错：比如传了 "blackegg/skills/..."，而实际项目是 "blackegg/me"。
	// 尝试把 clean 拆成 org/path，看该 org 下的哪个仓库有这个文件。
	if segs := strings.Split(clean, "/"); len(segs) >= 2 {
		org := segs[0]
		subPath := strings.Join(segs[1:], "/")
		var subMatch string
		subMatchCount := 0
		for _, p := range projects {
			if strings.HasPrefix(p, org+"/") {
				if r, rerr := s.projects.Get(p); rerr == nil {
					br := s.persona
					if br == "" {
						br = "zero"
					}
					if _, ferr := r.Read(br, subPath); ferr == nil {
						subMatch = p
						subMatchCount++
					}
				}
			}
		}
		if subMatchCount == 1 {
			return subMatch, subPath, nil
		}
	}

	// 2. 匹配不到：如果带了斜杠，且第一个段看起来像是一个未声明的项目，拒绝
	// 不要把 "notexist/foo.md" 吞成 pwd 下的 "notexist/foo.md"
	if strings.ContainsRune(clean, '/') && pwd != "" {
		// 只有当 clean 本身确实就是 pwd 自己的相对路径时才放行
		// 判断方式：看它的第一段是不是任何已知项目的前缀
		first, _, _ := strings.Cut(clean, "/")
		for _, p := range projects {
			if p == first || strings.HasPrefix(p, first+"/") {
				// 匹配到已知项目的上级或同名，说明调用方是想写全路径但写错了，直接拒绝
				return "", "", fmt.Errorf("项目 %q 未在配置里声明或地址写错", first)
			}
		}
	}

	// 3. 有 pwd 就当成 pwd 下的相对路径
	if pwd != "" {
		return pwd, clean, nil
	}

	// 4. 智能相对路径：模型经常只传 "skills/01-me/04-names.md"，
	// 不带 "blackegg/me/" 前缀。扫一遍已声明项目，如果恰好只有一个项目
	// 下存在这个文件，直接命中——不要逼模型猜仓库全名前缀。
	var matchProj string
	matchCount := 0
	for _, p := range projects {
		if r, rerr := s.projects.Get(p); rerr == nil {
			br := s.persona
			if br == "" {
				br = "zero"
			}
			if _, ferr := r.Read(br, clean); ferr == nil {
				matchProj = p
				matchCount++
			}
		}
	}
	if matchCount == 1 {
		return matchProj, clean, nil
	}
	if matchCount > 1 {
		return "", "", fmt.Errorf("相对路径 %q 在多个项目里都存在，请加项目前缀", clean)
	}

	return "", "", errors.New("地址应为 <项目>/<路径>（未匹配到已知项目且相对路径在任何仓库都不存在），得到 " + addr)
}

func (s *Server) projectOfAddr(addr string) string {
	clean := strings.TrimPrefix(strings.TrimSpace(addr), "/")
	if p, _, _, ok := memory.SplitID(clean); ok {
		return p
	}
	if p, _, ok := memory.SplitAddrWithProjects(clean, s.declaredProjects()); ok {
		return p
	}
	return ""
}

// projectOfBlock 返回块所属的项目名。
func projectOfBlock(b memory.Block) string { return b.Project }

// addrToVirtual 把内部地址（"project/path"）结合分支翻成虚拟地址。
func (s *Server) addrToVirtual(addr, branch string, c Caller) string {
	// 地址可能带分支（索引键 project@branch/path），先拆开。
	if p, b, path, ok := memory.SplitID(addr); ok {
		if branch == "" {
			branch = b
		}
		return c.Mount.ToVirtual(p, branch, path)
	}
	proj := s.projectOfAddr(addr)
	if proj == "" {
		return ""
	}
	path := strings.TrimPrefix(addr, proj+"/")
	return c.Mount.ToVirtual(proj, branch, path)
}
