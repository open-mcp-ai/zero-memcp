package server

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// 索引范围与进度（issue: 按分支控制索引 + 索引是动态过程）。
//
// # 两件事
//
//  1. **范围**：`[index] include/exclude` 决定哪些分支建索引。不建索引的
//     分支照样能 read / ls / log / grep（它们读的是仓库本身），只是不进
//     倒排表、不进正文缓存、不进向量表——那三样才是内存与启动时间的
//     大头（本机实测 89% 的块集中在 main.chat / main.record / inbox）。
//
//  2. **进度**：索引是动态过程，不是「启动完才有」。进程立即开始服务，
//     索引在后台一段一段建起来；期间 status 报进度，recall 明确说明
//     「现在只覆盖了已建好的一部分」。绝不允许半份索引静默地看起来像
//     全量——那会让调用方把「还没索引到」读成「记忆里没有」。
//
// # 为什么进度要能被看见
//
// 半份索引的危险不在结果少，而在结果少得**没有区别**：用户问「有没有
// 提过 X」，得到空结果，于是得出「没有」这个错误结论。这类静默失效比
// 慢、比报错都贵——它污染的是结论本身。

// indexProgress 是索引构建的进度快照。
type indexProgress struct {
	// Phase 是 "idle" / "scanning" / "building" / "ready" / "failed"。
	Phase string
	// Done / Total 是已建 / 待建的块数。Total 为 0 表示还没数出来。
	Done  int64
	Total int64
	// StartedAt / FinishedAt 让调用方能算出速度与预计剩余。
	StartedAt  time.Time
	FinishedAt time.Time
	// Err 非空表示后台构建失败（此时服务仍在跑，但索引不完整）。
	Err string
}

// Percent 返回 0-100；Total 未知时返回 -1（调用方不要把它当成 0%）。
func (p indexProgress) Percent() int {
	if p.Total <= 0 {
		return -1
	}
	n := int(p.Done * 100 / p.Total)
	if n > 100 {
		n = 100
	}
	return n
}

// Building 报告索引是否仍在构建中（即当前结果可能不完整）。
func (p indexProgress) Building() bool {
	return p.Phase == "scanning" || p.Phase == "building"
}

// Line 是一行人类可读的进度描述，status 与 recall 都用它。
func (p indexProgress) Line() string {
	switch p.Phase {
	case "ready":
		return fmt.Sprintf("index: ready (%d blocks)", p.Done)
	case "failed":
		return fmt.Sprintf("index: FAILED after %d/%d blocks: %s", p.Done, p.Total, p.Err)
	case "scanning":
		return fmt.Sprintf("index: scanning repositories (blocks counted so far: %d)", p.Done)
	case "building":
		if p.Total > 0 {
			el := time.Since(p.StartedAt).Round(time.Second)
			return fmt.Sprintf("index: building %d/%d blocks (%d%%) - results cover only what is built so far; elapsed %s",
				p.Done, p.Total, p.Percent(), el)
		}
		return fmt.Sprintf("index: building (%d blocks so far)", p.Done)
	default:
		return "index: idle"
	}
}

// setIndexProgress 发布一份进度（读者永远只看得到完整快照）。
func (s *Server) setIndexProgress(p indexProgress) {
	s.indexProg.Store(&p)
}

// indexProgressNow 取当前进度。
func (s *Server) indexProgressNow() indexProgress {
	if p := s.indexProg.Load(); p != nil {
		return *p
	}
	return indexProgress{Phase: "idle"}
}

// branchIndexed 报告某条分支是否参与索引。
//
// 判据来源是**当前生效的配置**（cfgFilePtr 会被 config put 热替换），
// 所以「改配置 → 立即生效」不需要重启：Reload 会按新范围重建。
func (s *Server) branchIndexed(project, branch string) bool {
	if !s.indexEnabled {
		return false
	}
	if f := s.cfgFilePtr.Load(); f != nil {
		return f.Index.MatchBranch(project, branch)
	}
	// 没有配置文件（命令行构造）时按「全建」——Config 里没有逐分支字段，
	// 而库用户与测试构造的实例本来就只关心「开还是关」。
	return true
}

// indexScopeNote 生成一行「哪些分支没建索引」的说明，供 status 使用。
//
// 它必须存在：一个被排除的分支，在 recall 里表现为「搜不到」。
// 如果没有任何地方说明「它被排除了」，那这个配置项本身就是一台
// 制造错误结论的机器。
func (s *Server) indexScopeNote() string {
	f := s.cfgFilePtr.Load()
	if f == nil {
		return ""
	}
	inc, exc := f.Index.Include, f.Index.Exclude
	if len(inc) == 0 && len(exc) == 0 {
		return ""
	}
	// 统计实际被排除的分支数与块数：说「配了 exclude」不够，
	// 要说「因此有多少块没进索引」。
	var excludedBranches, excludedBlocks int
	for _, tr := range s.treeList() {
		if s.branchIndexed(tr.Project, tr.Branch) {
			continue
		}
		excludedBranches++
		excludedBlocks += len(tr.Blocks)
	}
	if len(inc) > 0 {
		return fmt.Sprintf("index scope: whitelist %v (%d branch(es), %d blocks NOT indexed; read/ls/log/grep still work on them)",
			inc, excludedBranches, excludedBlocks)
	}
	return fmt.Sprintf("index scope: blacklist %v (%d branch(es), %d blocks NOT indexed; read/ls/log/grep still work on them)",
		exc, excludedBranches, excludedBlocks)
}

// excludedScopeError 在调用方显式指名一条未建索引的分支时给出明确拒绝。
//
// 为什么必须是错误而不是「没有命中」：`recall x --scope p@main.chat`
// 得到空结果，调用方会得出「聊天记录里没提过 X」——而真相是这条分支
// 根本没进索引。这类错误结论会一路被引用下去。
func (s *Server) excludedScopeError(scope string, c Caller) error {
	proj, branch, _, err := s.resolveAddr(scope, c)
	if err != nil || branch == "" {
		return nil
	}
	if s.branchIndexed(proj, branch) {
		return nil
	}
	return invalidf(map[string]any{"scope": scope, "project": proj, "branch": branch, "reason": "excluded_from_index"},
		"%s@%s is excluded from indexing by [index] include/exclude; recall cannot see it.\n"+
			"read/ls/log/grep still work on it (they read the repository, not the index).\n"+
			"To index it, update [index] via `config put` (applies immediately, no restart).",
		proj, branch)
}

// indexCounters 是后台构建期间对外的原子计数。
//
// 用 atomic 而不是 mutex：与 index.Index 的纪律一致——读侧永不阻塞写侧。
type indexCounters struct {
	done  atomic.Int64
	total atomic.Int64
}

// indexScopeFingerprint 是当前 [index] 范围的稳定指纹。
//
// 进快照头，用来判断「这份快照还是不是当前范围建的」。
func (s *Server) indexScopeFingerprint() string {
	f := s.cfgFilePtr.Load()
	if f == nil {
		return "all"
	}
	inc := append([]string(nil), f.Index.Include...)
	exc := append([]string(nil), f.Index.Exclude...)
	sort.Strings(inc)
	sort.Strings(exc)
	return "inc=" + strings.Join(inc, ",") + ";exc=" + strings.Join(exc, ",")
}
