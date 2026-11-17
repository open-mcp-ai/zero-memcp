package server

import (
	"fmt"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// execWrite 实现 write 动词：把一段文本写进指定路径。
//
//	write <project>[@<branch>]/<path> -- <内容>
//
// 与 put 的区别：put 写的是「切片」（自动生成时间戳文件名、自动加
// frontmatter），write 写的是「这个路径上就该是这段内容」。
// 它存在的理由是规则文件：SKILL.md 的 frontmatter 决定注入与索引，
// 而那不可能靠 put 的自动命名表达。
//
// 仅 root：一个能改 SKILL.md 的人可以改「谁看见什么」，
// 那比改内容本身危险。
// execWrite 把内容写到**指定路径**（区别于 put：put 生成时间戳文件名）。
//
// # 权限模型
//
// 这里曾经硬性要求 root。放开它是因为「让调用方维护自己的知识结构」
// 这件事必须能写指定路径——而 root 门槛把这个能力锁给了管理员。
//
// 放开之后仍然逐条走 ACL：
//
//   - requireBranchWide：这条分支在凭证里必须是整分支可见，
//     不能是被子树收窄过的（否则「写 SKILL.md」这种根路径操作无从判定）；
//   - allowed(..., configBitWrite)：必须对这条分支有 w 权限。
//
// 于是能力边界是：**有某条分支的 w 权限 = 能改这条分支上任何文件**。
// 这是刻意扩大的范围：早先 w 只够 append 新记忆（put 生成新名字，
// 覆盖不了已有文件），现在包含改写与新建。
//
// 风险与代价（不藏）：
//   - 拿 w 的调用方能改 SKILL.md 这类注入系统提示的文件，等于能改写
//     自己的身份约束；
//   - 缓解不是靠这里加白名单（那会把「改哪些文件」写死进服务端），
//     而是靠 git 历史可回溯 + 规范化地址可核对 diff。
func (s *Server) execWrite(rest string, c Caller, defaultBranch string) (string, error) {

	addr, content, ok := strings.Cut(rest, "--")
	if !ok {
		return "", fmt.Errorf("usage: write <project>[@<branch>]/<path> -- <content>")
	}
	// 只吃掉紧跟 -- 的那一个空格，其余原样保留。
	//
	// 为什么不是 TrimSpace：内容里可能有需要保留的首行缩进。
	// 为什么必须吃掉这一个：`write x -- ---\nfrontmatter...` 是给人
	// 敲的，中间那个空格是分隔符的一部分，不是内容的第一行。
	// 曾经完全不动，结果每个文件开头多一个空格，hasFrontmatter 的
	// HasPrefix(data, "---") 判定失败，inject: every_turn 全部失效——
	// 写成功了、也索引了，但一个字都不注入，静默得很彻底。
	content = strings.TrimPrefix(content, " ")
	// 正文可能是通过 $BODY 传进来的原子词，必须在这里还原。
	//
	// 漏掉这一行的后果不是「格式不对」：marker 会原样落盘，
	// 用户看到的是 "~ZMEMARG~5rOV...~END~"，而写入回显还报「已写入 xx 字节」。
	// 数据没丢，但内容变成了噪声，且没有任何一处报错。
	content = restorePayload(content)
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("write path is required")
	}

	proj, branch, path, err := s.resolveAddr(addr, c)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("%q specifies a project but no file path", addr)
	}
	if !s.allowed(c, proj, branch, configBitWrite) {
		return "", fmt.Errorf("not authorized to write to %s@%s", proj, branch)
	}

	// 写入路径必须和 put 一致：项目已声明且这次写入获准，就不该因为
	// 「仓库目录还没建」而失败。早先用 Get（读语义，不建仓），于是
	// 同一个目标 put 能写、write 报「仓库不存在」，而 write 正是
	// 写文件那个动词。Ensure 只在这里调用：读路径仍然不建仓。
	repo, err := s.projects.Ensure(proj)
	if err != nil {
		return "", err
	}

	// 内容原样写：不做 trim，保留下游可能依赖的排版
	if err := repo.Write(branch, path, []byte(content), gitstore.ModeContent); err != nil {
		return "", err
	}
	// 只刷新这一个文件，不重扫整条分支。
	//
	// 演进过程：无条件 s.Reload()（写 25 字节要 181.5 秒）
	//   → refreshBranch（只刷一条分支，但在 main.chat 这种 5233 文件的
	//     分支上仍要 19 秒，因为整条分支被重读重分词）
	//   → refreshPath（只动这一个 path，O(路径深度)）。
	//
	// 语义没变：都是让索引反映当前磁盘状态。变的是工作量的量级——
	// 前两者都随分支规模增长，后者不随。
	if err := s.refreshPath(proj, branch, path, true); err != nil {
		return "", fmt.Errorf("written, but failed to refresh the index: %w", err)
	}
	return fmt.Sprintf("written to %s@%s/%s (%d bytes); index refreshed", proj, branch, path, len(content)), nil
}
