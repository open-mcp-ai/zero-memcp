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
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/langdetect"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

const (
	rrfK        = 60.0
	srecallTopN = 5

	// srecallLayerN 是**每一层**各自的候选配额（Issue #13）。
	//
	// 早先只有一路 BM25 取 8 条：那次讨论的 thought 逐字复述查询词、
	// 篇幅又长，一次就把 8 个名额铺满，跨日期的 record 连名单都进不了。
	// 按层各留 N 条后，record 不需要越过 thought 堆才能露头。
	srecallLayerN = 8
)

// layerOrder 是参与分层 RRF 的层名，同时也是路径前缀。
//
// 与 semantic.go 的 isSemanticContent 保持同一套词：层是写在路径上的
// （`record/YYYY-MM-DD/…`），不是写在正文里的——thought 切片正文里有
// 一层 `layer: thought`，record 没有，只看正文会把 record 全归成空层。
var layerOrder = []string{"fact", "record", "review", "thought"}

// layerOfAddr 从物理地址（project@branch/path）里判层。
//
// 判不出来返回 ""（根级切片、导入的旧数据）。它会被当成独立一层：
// 无名层里也有自己的第 1 名，不会因为“不知道属于哪层”就被挤掉。
func layerOfAddr(addr string) string {
	_, _, path, ok := memory.SplitID(addr)
	if !ok {
		return ""
	}
	for _, l := range layerOrder {
		if path == l || strings.HasPrefix(path, l+"/") {
			return l
		}
	}
	return ""
}

func (s *Server) execSRecall(rest string, c Caller) (TermResult, error) {
	// 计时起点：tookMs 要覆盖向量路 + 关键词路 + 融合去重全过程。
	tStart := time.Now()
	rest = restorePayload(rest)
	// --json 先摘掉：它是返回形态，不是查询词。
	rest, wantJSON := stripJSONFlag(rest)
	if bad, ok := unknownFlag(rest); ok {
		return TermResult{}, fmt.Errorf("srecall: unknown flag %q (supported: --scope, --json)", bad)
	}
	query, scope := splitScope(rest)
	if query == "" {
		return TermResult{}, fmt.Errorf("srecall syntax: srecall <text> [--scope <tree>]")
	}
	if scope != "" {
		if err := s.excludedScopeError(scope, c); err != nil {
			return TermResult{}, err
		}
	}

	// 0) 语言判定：决定先查哪个索引，并作为审计字段回显。
	//
	// 为什么必须回显：多索引并存时，同一条查询在中文索引与英文索引下
	// 分数天然不同（不同模型的空间不可比）。没有 query_lang，
	// 「昨天同样的问法结果不一样」就无法解释。
	qLang := langdetect.Detect(query)

	// 1) 向量路：多索引并存时各查各的再按名次融合，单索引退回原路径
	vHits := s.semanticSearchAll(query)

	// 2) BM25 路：照常在当前凭证可见范围内跑
	allowedHit := func(addr string) bool {
		proj, br, _, ok := memory.SplitID(addr)
		if !ok {
			proj = s.projectOfAddr(addr)
			br = s.defaultBranch
		}
		if !s.branchVisible(c, proj, br) {
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
	// 每层配额：配额用完就不再放行该层，让后面的层有自己的名额。
	//
	// 过滤器按**分数降序**走，所以每层被放行的前 N 条就是该层
	// 自己的 top-N——一次扫描就能拿到分层榜单，不必扫三遍。
	layerQuota := map[string]int{}
	bmHits := s.idxNow().SearchCovered(query, srecallLayerN*8, 0, recallMinCoverage, s.weightCount, func(addr string) bool {
		if !allowedHit(addr) || !scopeMatch(addr) {
			return false
		}
		l := layerOfAddr(addr)
		if layerQuota[l] >= srecallLayerN {
			return false
		}
		layerQuota[l]++
		return true
	})

	// 3) RRF 融合：按排名计分，与两路的原始分数尺度无关。
	//
	// 实测教训：早先用「语义先占 3 席、BM25 补后 2 席」的顺序融合，
	// 结果 1729 案的 L2 种子（BM25 第 1、9.31 分）被语义的压缩噪声
	// （0.84~0.85）挤出局。排名融合让两路各按名次贡献，双路命中
	// 自动叠加——这正是「L2 在 BM25 第 1 + 语义第 25」想要的形状。
	//
	// 分层再融一次（Issue #13）：光分两路不够，因为每一路里
	// 层与层仍然**同台竞争名次**。thought 是推理过程，逐字复述
	// 查询词的概率极高、篇幅又长，在 BM25 里天然吃掉靠前的名次；
	// 一次讨论就能把 top-N 铺满，跨日期的 record 根本进不了名单。
	//
	// 改法不是给 thought 乘一个魔法系数，而是把「层」当第三个维度
	// 拆成独立榜单：每层的第 1 名都拿 1/(k+1)，名次只在**同层内**
	// 比较。这样一条 record 不再需要越过一堆 thought 才能露头——
	// 它只需要是 record 里最相关的那个。
	//
	// 为什么不用加权：力度靠人拍，而 RRF 的力度由名次自己决定。
	// 真要调，也只调路由权重，不是给某一层编一个数字。
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

	// addRoute 把一路候选按层拆开，层内重新从 1 开始给名次。
	//
	// 入参已按该路分数降序，所以同层内出现的顺序就是该层的名次。
	addRoute := func(route string, addrs []string) {
		perLayer := map[string]int{}
		for _, a := range addrs {
			l := layerOfAddr(a)
			perLayer[l]++
			rrf[a] += 1.0 / (rrfK + float64(perLayer[l]))
			if prev, ok := src[a]; ok && prev != route {
				src[a] = "both"
			} else if !ok {
				src[a] = route
			}
		}
	}

	// 向量路：vHits 已按分数降序
	vAddrs := make([]string, 0, len(vHits))
	for _, vh := range vHits {
		if !allowedHit(vh.ID) || !scopeMatch(vh.ID) {
			continue
		}
		vAddrs = append(vAddrs, vh.ID)
	}
	addRoute("semantic", vAddrs)

	// BM25 路：已经是每层各自 top-N（配额在检索时就限住了）
	bmAddrs := make([]string, 0, len(bmHits))
	for _, bh := range bmHits {
		bmAddrs = append(bmAddrs, bh.Addr)
	}
	addRoute("lexical", bmAddrs)

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
		empty := map[string]any{
			"hits": []any{}, "results": []any{}, "count": 0,
			"query": query, "tookMs": time.Since(tStart).Milliseconds(),
			"query_lang": string(qLang.Lang), "query_zh_ratio": qLang.ZHRatio,
			"semantic_route": s.semanticRouteLabel(),
		}
		if wantJSON {
			return TermResult{Text: jsonText(empty), Structured: empty, Empty: true, EmptyReason: string(RefusalNotFound)}, nil
		}
		return TermResult{Text: "no matches", Structured: empty, Empty: true, EmptyReason: string(RefusalNotFound)}, nil
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

	structured := map[string]any{
		"hits":    structHits,
		"results": structHits, // 同 recall：别名，指向同一份数据
		"count":   len(structHits),
		// 审计字段，与 recall 对齐：调用方要把「哪条消息、多少分、
		// 放行还是拒绝」串起来，回显和耗时是拼这条链的两颗钉子。
		"query":  query,
		"tookMs": time.Since(tStart).Milliseconds(),
		// 多索引并存时的审计三件套：问了什么、判成什么语言、查了哪一路。
		"query_lang":     string(qLang.Lang),
		"query_zh_ratio": qLang.ZHRatio,
		"semantic_route": s.semanticRouteLabel(),
	}
	if wantJSON {
		return TermResult{Text: jsonText(structured), Structured: structured}, nil
	}
	return TermResult{
		Text:       strings.TrimRight(sb.String(), "\n"),
		Structured: structured,
	}, nil
}
