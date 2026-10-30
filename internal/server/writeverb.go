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
// 与 note 的区别：note 写的是「切片」（自动生成时间戳文件名、自动加
// frontmatter），write 写的是「这个路径上就该是这段内容」。
// 它存在的理由是规则文件：SKILL.md 的 frontmatter 决定注入与索引，
// 而那不可能靠 note 的自动命名表达。
//
// 仅 root：一个能改 SKILL.md 的人可以改「谁看见什么」，
// 那比改内容本身危险。
func (s *Server) execWrite(rest string, c Caller, persona string) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}

	addr, content, ok := strings.Cut(rest, "--")
	if !ok {
		return "", fmt.Errorf("用法：write <project>[@<branch>]/<path> -- <内容>")
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
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("缺写入路径")
	}

	proj, branch, path, err := s.resolveAddr(addr, c)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("%q 只给出了项目，没有文件路径", addr)
	}
	if !s.allowed(c, proj, branch, configBitWrite) {
		return "", fmt.Errorf("无权写入 %s@%s", proj, branch)
	}

	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}

	// 内容原样写：不做 trim，保留下游可能依赖的排版
	if err := repo.Write(branch, path, []byte(content), gitstore.ModeContent); err != nil {
		return "", err
	}
	if err := s.Reload(); err != nil {
		return "", fmt.Errorf("已写入，但重建索引失败：%w", err)
	}
	return fmt.Sprintf("已写入 %s@%s/%s（%d 字节），索引已重建", proj, branch, path, len(content)), nil
}
