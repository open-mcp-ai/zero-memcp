package server

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/open-mcp-ai/zero-memcp/internal/embed"
	"github.com/open-mcp-ai/zero-memcp/internal/vecindex"
)

// 多索引运行时（live index registry）。
//
// 上一版只做到了**落盘隔离**：每个模型一个目录，互不覆盖。
// 那是两件事里的一半——目录能共存，不代表进程能同时用。
// semanticState.active 是单个原子指针，意味着同一时刻只有一套索引活着，
// 另一套目录只是躺在硬盘上。
//
// 这个文件补上另一半：进程内可以持有 N 套索引，每套有自己的
// 嵌入器（各模型的向量空间不可比，查询必须用产生该索引的那个模型
// 重新算一遍）、自己的维度、自己的语言标注。
//
// 合并的唯一合法方式是**排名融合（RRF）**：
// 中文模型的 0.62 与英文模型的 0.41 之间没有可比性——余弦相似度
// 只在同一空间内有意义。比分数是错的，比名次才对。

// liveIndex 是一套已经加载、可以被查询的向量索引。
type liveIndex struct {
	// key 是落盘子目录名（vecindex.ModelKeyFor 推导），用于去重与诊断。
	key string
	// model 是模型标识，进 manifest 与 status。
	model string
	// rev 是权重修订号：同名但换了权重必须分属不同索引。
	rev string
	// lang 是这套索引专长的语言（"zh"/"en"/"" 表示通用）。
	// 它只影响诊断与未来路由，不改变「两套都查再按名次融合」的策略——
	// 猜语言去跳过某一路，会让「中文夹英文标识符」这类查询漏掉命中。
	lang string
	dir  string
	emb  embed.Embedder
	// ix 是可被后台重建整体替换的原子槽。
	ix atomic.Pointer[vecindex.Index]
}

func (li *liveIndex) index() *vecindex.Index { return li.ix.Load() }

func (li *liveIndex) store(ix *vecindex.Index) { li.ix.Store(ix) }

func (li *liveIndex) ready() bool { return li != nil && li.ix.Load() != nil && li.emb != nil }

// liveIndexes 是进程内的多索引表。
//
// 用 sync.Map 而不是加锁：每个 key 独立，没有跨 key 不变量。
type liveIndexes struct {
	m sync.Map // key -> *liveIndex
	n atomic.Int64
}

func (r *liveIndexes) put(li *liveIndex) {
	if _, loaded := r.m.LoadOrStore(li.key, li); !loaded {
		r.n.Add(1)
	}
}

func (r *liveIndexes) all() []*liveIndex {
	var out []*liveIndex
	r.m.Range(func(_, v any) bool {
		if li, ok := v.(*liveIndex); ok && li.ready() {
			out = append(out, li)
		}
		return true
	})
	// 稳定顺序：按 key 排序，让「同样的查询两次结果不同」可归因。
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func (r *liveIndexes) count() int { return int(r.n.Load()) }

// loadLiveIndexes 按配置为每个模型载入一套索引。
//
// 只载入**已经建好**的目录：建索引要调嵌入服务、按份数呈线性成本，
// 而且必须在后台跑。这里只做「有就挂上」，缺的那份由后台追平逻辑负责。
// 这样做的直接好处是：进程启动不会被第二、第三个模型的构建卡住。
func (s *Server) loadLiveIndexes() {
	if s.liveIdx == nil {
		return
	}
	providers := s.fileSemanticProviders()
	seen := map[string]bool{}
	// 主链当前用的嵌入器：它已经活着，复用它就不必再 open 一次。
	// 少 open 一次不只是省时间——每次 OpenChain 的 HTTP 后端都要先
	// 探一次维度，那是一次真实的网络往返，会污染调用计数与延迟曲线。
	primaryEmb := s.semantic.getEmb()
	for _, p := range providers {
		if p.Model == "" || p.Kind == "" {
			continue
		}
		key := vecindex.ModelKeyFor(p.Model, p.ModelRev)
		if seen[key] {
			continue
		}
		seen[key] = true

		var emb embed.Embedder
		if primaryEmb != nil && primaryEmb.Name() == p.Model {
			emb = primaryEmb
		} else {
			// 门禁：磁盘上还没有这份索引就别急着开嵌入器。
			// 没有缓存 → 没有可挂的索引 → 开了也是白探一次，
			// 而「白探」在网络不通时会拖慢启动。
			guess := vecindex.ResolveDir(s.semanticDir, p.Model, p.ModelRev)
			if p.IndexDir != "" {
				guess = s.semanticDir + "/" + p.IndexDir
			}
			if _, serr := os.Stat(guess + "/manifest.json"); serr != nil {
				s.logf("semantic: multi-index %s: no cache at %s yet (will be built on demand)", p.Model, guess)
				continue
			}
			opened, oerr := embed.OpenChain(embed.ChainConfig{Providers: []embed.ProviderConfig{{
				Label:     p.Label,
				Kind:      p.Kind,
				Endpoint:  p.Endpoint,
				Model:     p.Model,
				Dim:       p.Dim,
				APIKey:    p.APIKey,
				Timeout:   p.Timeout,
				LibPath:   p.LibPath,
				ModelPath: p.ModelPath,
				VocabPath: p.VocabPath,
				ModelRev:  p.ModelRev,
			}}})
			if oerr != nil {
				s.logf("semantic: multi-index %s (%s): embedder unavailable: %v", p.Model, p.Kind, oerr)
				continue
			}
			emb = opened
		}
		dir := vecindex.ResolveDir(s.semanticDir, emb.Name(), embed.Revision(emb))
		if p.IndexDir != "" {
			dir = s.semanticDir + "/" + p.IndexDir
		}
		ix, _, lerr := vecindex.LoadRev(dir, emb.Name(), embed.Revision(emb))
		if lerr != nil {
			// 出声：目录里没东西不是错误（首次运行必然如此），
			// 但必须让人知道「这套索引现在查不到东西」。
			s.logf("semantic: multi-index %s: no cache at %s yet (will be built on demand)", p.Model, dir)
			continue
		}
		if ix.Dim() != emb.Dim() {
			s.logf("semantic: multi-index %s: cache dim %d != model dim %d, ignoring", p.Model, ix.Dim(), emb.Dim())
			continue
		}
		li := &liveIndex{key: vecindex.ModelKeyFor(emb.Name(), embed.Revision(emb)), model: emb.Name(), rev: embed.Revision(emb), lang: p.Lang, dir: dir, emb: emb}
		li.store(ix)
		s.liveIdx.put(li)
		s.logf("semantic: multi-index ready %s (%d vectors, dim=%d, lang=%s)",
			emb.Name(), ix.Len(), ix.Dim(), p.Lang)
	}
	if n := s.liveIdx.count(); n > 0 {
		s.logf("semantic: %d live index(es) registered", n)
	}
}

// semanticSearchAll 在多套索引上各查一次，再用排名融合合并结果。
//
// 为什么不是「先判语言、只查那一路」：本项目最常见的查询是
// 「中文叙述夹英文标识符」，按语言硬路由一定会漏。两路都查 + 按名次
// 融合，漏掉的概率最小，代价只是多一次嵌入调用。
func (s *Server) semanticSearchAll(query string) []vecindex.Hit {
	lists := make([][]vecindex.Hit, 0, 2)
	cands := s.liveIdx.all()
	if len(cands) == 0 {
		// 回落到上一版的单索引路径（老部署没有 [[semantic.provider]] 列表）。
		if hits := s.semanticSearch(query); len(hits) > 0 {
			return hits
		}
		return nil
	}
	for _, li := range cands {
		qv, err := li.emb.Embed(query)
		if err != nil {
			s.logf("semantic: index %s skipped this query: %v", li.model, err)
			continue
		}
		hits := li.index().Search(qv, 30, semanticMinScore)
		if len(hits) > 0 {
			lists = append(lists, hits)
		}
	}
	if len(lists) == 0 {
		return nil
	}
	if len(lists) == 1 {
		return lists[0]
	}
	return mergeRRF(lists)
}

// mergeRRF 把多路结果按名次融合。
//
// 输入是各路自己的排序（互不可比的分数），输出是按融合分排序的列表。
// 返回值里的 Score 是融合分，**不是相似度**——调用方不应对它设阈值，
// 阈值判定必须发生在各路内部（那才是同一空间内的比较）。
func mergeRRF(lists [][]vecindex.Hit) []vecindex.Hit {
	const k = 60.0
	score := map[string]float64{}
	for _, list := range lists {
		for i, h := range list {
			score[h.ID] += 1.0 / (k + float64(i+1))
		}
	}
	out := make([]vecindex.Hit, 0, len(score))
	for id, sc := range score {
		out = append(out, vecindex.Hit{ID: id, Score: float32(sc)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// liveIndexLabel 给 status / srecall 回显：这套查询实际走了哪些模型。
func (s *Server) liveIndexLabel() string {
	if s.liveIdx == nil {
		return ""
	}
	all := s.liveIdx.all()
	if len(all) == 0 {
		return ""
	}
	parts := make([]string, 0, len(all))
	for _, li := range all {
		parts = append(parts, fmt.Sprintf("%s(%d)", li.model, li.index().Len()))
	}
	return strings.Join(parts, ", ")
}
