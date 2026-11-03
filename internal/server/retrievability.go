// retrievability.go —— 取用面的三条兜底：跨分支读、输出上限、正文时间。
//
// 为什么这三件事放在一起：它们都出自同一批事故，而共同点是
// 「东西在库里，但取不出来」。存进去却取不出，等于没存。
//
//	1. read 不带 @branch 会 not found——用户手里只有路径，
//	   「默认分支是哪个」是他不该知道的实现细节；
//	2. ls <date> 全量倾倒 26 万字符打爆窗口——每一行只值 60 字节，
//	   却按完整通道计费；
//	3. log 按 commit 时间排序，同一批导入的切片时间相同，退化成
//	   路径字典序——于是"最近"其实是"文件名最大的那批"。

package server

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// lsMaxEntries 是 ls 一次最多列出的条目数。
//
// 50 的依据：一屏约 50 行；超过就说明查询太宽，该收窄而不是翻页。
const lsMaxEntries = 50

// branchCandidates 在「没写 @branch 且默认分支读不到」时，
// 列出该项目下确实存在这个路径的分支名。
//
// 权限逐条走 allowed()：跨分支兜底不能成为绕过可见性的后门。
// 返回的是短名（refs/heads/record → record），可直接拼回地址。
func (s *Server) branchCandidates(repo *gitstore.Repo, path string, c Caller, proj string) []string {
	refs, err := repo.Refs("refs/heads/")
	if err != nil {
		return nil
	}
	var out []string
	for _, ref := range refs {
		br := strings.TrimPrefix(ref, "refs/heads/")
		if br == ref {
			continue // 不是 heads 下的（如 tags），跳过
		}
		if !c.Mount.Visible(proj, br) || !s.allowed(c, proj, br, configBitRead) {
			continue
		}
		if _, rerr := repo.Read(br, path); rerr == nil {
			out = append(out, br)
		}
	}
	return out
}

// 正文里的时间戳：frontmatter 的 at:/created:，RFC3339 或带时区偏移。
var (
	reAt      = regexp.MustCompile(`(?m)^\s*(?:[-*]?\s*)?(?:at|created):\s*(\d{4}-\d{2}-\d{2}[T ][0-9:+\-.Z:]+)`)
	reDateDir = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)
	// 文件名里的紧凑时间：124946.679516-1986-laptop.md → 12:49:46.679516
	reFileTime = regexp.MustCompile(`(?:^|/)(\d{6})(?:\.(\d+))?[^/]*\.md$`)
)

// sliceAtTime 解析一条切片真正代表的事件时间。
//
// 优先正文 frontmatter（at:/created:），其次路径里的日期目录 + 文件名时间，
// 最后回退 commit 时间（返回 0 表示"正文里没有，调用方用自己的兜底"）。
//
// 为什么不能只用 commit 时间：批量导入会把所有历史切片打成同一个
// commit 时间戳，排序随即退化成路径字典序——症状是"看到的是最近，
// 其实是文件名最大的那批"，且不报错。
func sliceAtTime(content, path string) int64 {
	if m := reAt.FindStringSubmatch(content); m != nil {
		s := strings.Replace(m[1], " ", "T", 1)
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.Unix()
			}
		}
	}
	// frontmatter 没有 → 从地址重建：日期目录给天，文件名给时分秒。
	if dm := reDateDir.FindStringSubmatch(path); dm != nil {
		day, err := time.ParseInLocation("2006-01-02", dm[1], time.Local)
		if err != nil {
			return day.Unix()
		}
		base := day
		if tm := reFileTime.FindStringSubmatch(path); tm != nil {
			h, _ := strconv.Atoi(tm[1][0:2])
			mi, _ := strconv.Atoi(tm[1][2:4])
			sec, _ := strconv.Atoi(tm[1][4:6])
			base = time.Date(day.Year(), day.Month(), day.Day(), h, mi, sec, 0, day.Location())
		}
		return base.Unix()
	}
	return 0
}

// contentAtForLog 只为 log 的排序服务：读一次正文取事件时间。
// 失败时返回 0，由调用方回退 commit 时间。
func (s *Server) contentAtForLog(b memory.Block) int64 {
	data, err := s.readBlock(b)
	if err != nil {
		return 0
	}
	return sliceAtTime(string(data), b.Path)
}
