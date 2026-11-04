// srecall.go —— 语义增强召回：BM25（精确字面）+ 向量（跨语言意图）两路融合。
//
// 为什么不把 recall 直接改成向量：两点——
//  1. 插件和老脚本依赖 recall 的精确行为，改动破坏兼容；
//  2. 纯向量在精确符号（commit SHA、错误码、特定端口）上打不过 BM25。
//
// srecall 的策略是两路独立出候选，用 RRF（Reciprocal Rank Fusion）融合：
//
//	RRF(d) = Σ_routes 1/(k + rank_route(d)),  k = 60
//
// 为什么必须用排名而不是分数相加——实测过分数相加会翻车：
// BM25 分数区间是 0~几十（无界），LaBSE 余弦挤在 0.78~0.88。
// 直接相加时分数尺度大的那路会淹没另一路；做归一化又依赖当次
// 查询的分数分布，库一换分布就变，阈值全废。排名天然可比。
//
// 更要紧的是：两路都命中的条目分数自动叠加，这正是要的效果。
// 实测 1729 案：L2 种子（case-1729-v2）在 BM25 排第 1（9.31 分），
// 但语义只排第 25；纯语义的前 3 名都是 0.84~0.85 的压缩噪声。
// 用 RRF 后它拿到 1/61 + 1/85，压过单路第一的 1/61。
//
// k=60 是信息检索的标准值：削弱头部过度优势。k 越小越激进
// （第 1 名权重压倒一切），越大越平均。60 下第 1 名 ≈0.0164、
// 第 10 名 ≈0.0143，不会让单路第一名霸榜，也不让长尾噪声挤进来。

package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

const (
	rrfK        = 60.0
	srecallTopN = 5
)

func (s *Server) execSRecall(rest string, c Caller) (TermResult, error) {
	rest = restorePayload(rest)
	if bad, ok := unknownFlag(rest); ok {
		return TermResult{}, fmt.Errorf("srecall: unknown flag %q (supported: --scope)", bad)
	}
	query, scope := splitScope(rest)
	if query == "" {
		return TermResult{}, fmt.Errorf("srecall syntax: srecall <text> [--scope <tree>]")
	}

	// 1) 向量路：异步搜，只在启用且就绪时走
	vHits := s.semanticSearch(query)

	// 2) BM25 路：照常在当前凭证可见范围内跑
	s.mu.RLock()
	allowedHit := func(addr string) bool {
		proj, br, _, ok := memory.SplitID(addr)
		if !ok {
			proj = s.projectOfAddr(addr)
			br = s.defaultBranch
		}
		if !c.Mount.Visible(proj, br) {
			return false
		}
		return s.allowed(c, proj, br, configBitRead)
	}
	scopeMatch := func(addr string) bool {
		if scope == "" {
			return true
		}
		return addrUnderScope(addr, scope)
	}
	bmHits := s.idx.SearchCovered(query, 8, 0, recallMinCoverage, s.weightCount, func(addr string) bool {
		return allowedHit(addr) && scopeMatch(addr)
	})

	// 3) RRF 融合：按排名计分，与两路的原始分数尺度无关。
	//
	// 实测教训：早先用「语义先占 3 席、BM25 补后 2 席」的顺序融合，
	// 结果 1729 案的 L2 种子（BM25 第 1、9.31 分）被语义的压缩噪声
	// （0.84~0.85）挤出局。排名融合让两路各按名次贡献，双路命中
	// 自动叠加——这正是「L2 在 BM25 第 1 + 语义第 25」想要的形状。
	type finalHit struct {
		addr      string  // 实际物理地址
		virt      string  // 虚拟地址
		score     float64 // RRF 融合分
		source    string  // "semantic" | "lexical" | "both"
		snippet   string
		companion string // 伴随切片地址（如 thought 带出同轮 record）
	}

	rrf := map[string]float64{}
	src := map[string]string{}

	// 向量路：vHits 已按分数降序
	for i, vh := range vHits {
		if !allowedHit(vh.ID) || !scopeMatch(vh.ID) {
			continue
		}
		rrf[vh.ID] += 1.0 / (rrfK + float64(i+1))
		src[vh.ID] = "semantic"
	}
	// BM25 路：多取几个（8），它的中段与语义命中叠加后可能反超
	for i, bh := range bmHits {
		rrf[bh.Addr] += 1.0 / (rrfK + float64(i+1))
		if prev, ok := src[bh.Addr]; ok && prev == "semantic" {
			src[bh.Addr] = "both"
		} else if _, ok := src[bh.Addr]; !ok {
			src[bh.Addr] = "lexical"
		}
	}

	// 排序取 top-N，再补齐展示信息（snippet/companion 要读树，锁内做）。
	type ranked struct {
		addr string
		rrf  float64
		src  string
	}
	rlist := make([]ranked, 0, len(rrf))
	for a, sc := range rrf {
		rlist = append(rlist, ranked{a, sc, src[a]})
	}
	sort.Slice(rlist, func(i, j int) bool { return rlist[i].rrf > rlist[j].rrf })
	if len(rlist) > srecallTopN {
		rlist = rlist[:srecallTopN]
	}

	result := make([]finalHit, 0, len(rlist))
	for _, r := range rlist {
		_, hbr, _, okBr := memory.SplitID(r.addr)
		if !okBr {
			hbr = s.defaultBranch
		}
		virt := s.addrToVirtual(r.addr, hbr, c)
		if virt == "" {
			continue
		}
		snippet := s.snippet(r.addr, query, 240)
		comp := ""
		if r.src != "lexical" {
			_, comp = s.companionRecord(r.addr)
		}
		result = append(result, finalHit{
			addr:      r.addr,
			virt:      virt,
			score:     r.rrf,
			source:    r.src,
			snippet:   snippet,
			companion: comp,
		})
	}
	s.mu.RUnlock()

	// 去重：
	//
	// 1) 同轮折叠：A 的 companion 正好是列表里的 B，则 B 已经被
	//    「↳ 同轮问答结论: B」带出，不该再作为独立命中占一个席位。
	//    顺序很重要：只有 A 排在 B 前面才删 B；反过来 B 在前就保留 B
	//    （它是分更高的一方），A 的 ↳ 指针变成对上方条目的引用，
	//    不重复正文。否则会把高分结论扔掉、只留下低分的推理链。
	//
	// 2) 摘要指纹去重：分卷切片（part 1/2）或多分支镜像经常有
	//    完全相同的开头 240 字节。正文摘要一模一样的只保留最高分一条。
	var deduped []finalHit
	consumed := map[string]bool{} // 被已保留条目的 companion 引用过的地址
	seenSnippets := map[string]bool{}
	for _, h := range result {
		if consumed[h.virt] || consumed[h.addr] {
			continue
		}
		normSnip := strings.TrimSpace(h.snippet)
		if normSnip != "" && seenSnippets[normSnip] {
			continue
		}
		if normSnip != "" {
			seenSnippets[normSnip] = true
		}
		if h.companion != "" {
			consumed[h.companion] = true
		}
		deduped = append(deduped, h)
	}
	result = deduped

	if len(result) == 0 {
		return TermResult{
			Text:       "no matches",
			Structured: map[string]any{"hits": []any{}, "count": 0},
			Empty:      true,
		}, nil
	}

	var sb strings.Builder
	structHits := make([]any, len(result))
	for i, rh := range result {
		tag := ""
		if rh.source == "semantic" {
			tag = " [semantic]"
		} else if rh.source == "both" {
			tag = " [semantic+keyword]"
		}
		sb.WriteString(fmt.Sprintf("%s (%.4f)%s\n", rh.virt, rh.score, tag))
		if rh.snippet != "" {
			for _, line := range strings.Split(rh.snippet, "\n") {
				sb.WriteString("    " + line + "\n")
			}
		}
		if rh.companion != "" {
			sb.WriteString(fmt.Sprintf("    ↳ 同轮问答结论: %s\n", rh.companion))
		}
		sb.WriteString("\n")

		m := map[string]any{
			"addr":    rh.virt,
			"score":   rh.score,
			"source":  rh.source,
			"snippet": rh.snippet,
		}
		if rh.companion != "" {
			m["companion"] = rh.companion
		}
		structHits[i] = m
	}

	return TermResult{
		Text: strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{
			"hits":  structHits,
			"count": len(structHits),
		},
	}, nil
}
