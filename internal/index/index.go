// Package index 把可索引的块变成倒排表。
//
// 只做一件事：词 → 块。没有语义、没有模型、没有网络。
// 复杂度全部花在「哪些词值得记」上，因为噪音词会让联想变成随机数。
package index

import (
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Token 是一个索引词。
//
// 中文按二元组切分（"记忆服务" → 记忆/忆服/服务），英文按词切分并小写化。
// 不做词性标注、不做同义词扩展——那需要模型，而这一层刻意不用模型。
type Token string

// postingEntry 是倒排表里的一条：某篇文档、词频。
type postingEntry struct {
	Addr string
	TF   int
}

// postingList 是一个词的倒排列表（不可变，发布后不再修改）。
type postingList struct {
	entries []postingEntry
	// byAddr 是 entries 的地址索引，供撤销时 O(1) 定位。
	// 只在留出余量后才建：短列表线性扫更快。
	byAddr map[string]int
}

// postingSlot 是一个词的写入槽：一槽令牌串行化对该词的追加/撤销，
// list 原子发布不可变切片。
//
// 为什么按词分段而不是整个索引一把锁：常见词有几千篇文档，
// 罕见词只有一篇。全局锁会让「写一个罕见词」等「写一个常见词」。
type postingSlot struct {
	tok  chan struct{}
	list atomic.Pointer[postingList]
}

func newPostingSlot() *postingSlot {
	s := &postingSlot{tok: make(chan struct{}, 1)}
	s.list.Store(&postingList{})
	return s
}

// acquire 取该词的写令牌。
func (ps *postingSlot) acquire() func() {
	select {
	case ps.tok <- struct{}{}:
		return func() { <-ps.tok }
	default:
	}
	// 慢路径：等到有上界，而不是无限挂住。
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case ps.tok <- struct{}{}:
		return func() { <-ps.tok }
	case <-t.C:
		// 超时也放行：索引写入不是事务，丢掉一次更新比让写者永久
		// 阻塞要好——下一次全量重建会补齐。
		return func() {}
	}
}

func (ps *postingSlot) load() *postingList {
	if l := ps.list.Load(); l != nil {
		return l
	}
	return &postingList{}
}

// add 往该词的倒排里加一条。返回是否有变化。
func (ps *postingSlot) add(addr string, tf int) {
	release := ps.acquire()
	defer release()
	cur := ps.load()
	if i, ok := cur.byAddr[addr]; ok {
		next := make([]postingEntry, len(cur.entries))
		copy(next, cur.entries)
		next[i] = postingEntry{Addr: addr, TF: tf}
		ps.list.Store(&postingList{entries: next, byAddr: cur.byAddr})
		return
	}
	// 线性找一遍（短列表）；没找到再追加。
	for i := range cur.entries {
		if cur.entries[i].Addr == addr {
			next := make([]postingEntry, len(cur.entries))
			copy(next, cur.entries)
			next[i].TF = tf
			ps.list.Store(&postingList{entries: next, byAddr: cur.byAddr})
			return
		}
	}
	next := make([]postingEntry, len(cur.entries), len(cur.entries)+1)
	copy(next, cur.entries)
	next = append(next, postingEntry{Addr: addr, TF: tf})
	var byAddr map[string]int
	if len(next) >= 8 {
		byAddr = make(map[string]int, len(next))
		for i, e := range next {
			byAddr[e.Addr] = i
		}
	}
	ps.list.Store(&postingList{entries: next, byAddr: byAddr})
}

// addMany 一次写入该词的多个 (addr, tf)，只做一次拷贝。
//
// # 为什么必须有它
//
// entries 是不可变切片，每次 add 都要整份复制（那是为了让读侧免锁）。
// 单条 add 在活系统里没问题——一次只加一篇。但冷建是「N 篇文档共享
// 同一批常见词」，逐条 add 就退化成 O(N²)：一个出现在全部 5,000 篇里的
// 中文二元组，累计复制 0+1+…+4,999 ≈ 1,250 万条记录。
//
// 实测 5,000 篇冷建：分词 20ms，倒排写入 7.04 秒（348 倍）。也就是说
// 分词从来不是瓶颈，全耗在重复拷贝上。
//
// addMany 把「该词在这批里的所有文档」一次合并进去：每词一次令牌、
// 一次分配、一次拷贝，总量降到 O(N)。
func (ps *postingSlot) addMany(pairs []postingEntry) {
	if len(pairs) == 0 {
		return
	}
	release := ps.acquire()
	defer release()

	cur := ps.load()
	// 已存在同一 addr 的不该出现（冷建时索引为空），但活系统合并写入
	// 可能遇到，按「后写覆盖」处理，与 add 的语义一致。
	merged := make([]postingEntry, 0, len(cur.entries)+len(pairs))
	updated := make(map[string]int, len(pairs))
	for _, e := range pairs {
		updated[e.Addr] = e.TF
	}
	seen := make(map[string]bool, len(pairs))
	for _, e := range cur.entries {
		if tf, ok := updated[e.Addr]; ok {
			merged = append(merged, postingEntry{Addr: e.Addr, TF: tf})
			seen[e.Addr] = true
			continue
		}
		merged = append(merged, e)
	}
	for _, e := range pairs {
		if seen[e.Addr] {
			continue
		}
		merged = append(merged, e)
	}

	var byAddr map[string]int
	if len(merged) >= 8 {
		byAddr = make(map[string]int, len(merged))
		for i, e := range merged {
			byAddr[e.Addr] = i
		}
	}
	ps.list.Store(&postingList{entries: merged, byAddr: byAddr})
}

// remove 从该词的倒排里删掉一条。返回该词是否已空。
func (ps *postingSlot) remove(addr string) bool {
	release := ps.acquire()
	defer release()
	cur := ps.load()
	pos := -1
	if cur.byAddr != nil {
		if i, ok := cur.byAddr[addr]; ok {
			pos = i
		}
	} else {
		for i := range cur.entries {
			if cur.entries[i].Addr == addr {
				pos = i
				break
			}
		}
	}
	if pos < 0 {
		return len(cur.entries) == 0
	}
	next := make([]postingEntry, 0, len(cur.entries)-1)
	next = append(next, cur.entries[:pos]...)
	next = append(next, cur.entries[pos+1:]...)
	var byAddr map[string]int
	if len(next) >= 8 {
		byAddr = make(map[string]int, len(next))
		for i, e := range next {
			byAddr[e.Addr] = i
		}
	}
	ps.list.Store(&postingList{entries: next, byAddr: byAddr})
	return len(next) == 0
}

// removeMany 一次撤掉该词的多个 addr，只做一次拷贝。
//
// 与 remove 的区别就是拷贝次数：remove 撤一个 addr 就整份复制一遍
// entries，而「换掉一整条分支」要撤掉两万篇共同命中的常见词——
// 实测 20,448 篇逐条 remove 要 4m33s，按词聚合后是同一个量级里的
// 零头。语义与 remove 一致：撤掉的 addr 不存在时不算错。
func (ps *postingSlot) removeMany(gone map[string]bool) bool {
	if len(gone) == 0 {
		return false
	}
	release := ps.acquire()
	defer release()
	cur := ps.load()
	next := make([]postingEntry, 0, len(cur.entries))
	for _, e := range cur.entries {
		if gone[e.Addr] {
			continue
		}
		next = append(next, e)
	}
	if len(next) == len(cur.entries) {
		// 一个都没命中：不写回，也不复制。
		return len(cur.entries) == 0
	}
	var byAddr map[string]int
	if len(next) >= 8 {
		byAddr = make(map[string]int, len(next))
		for i, e := range next {
			byAddr[e.Addr] = i
		}
	}
	ps.list.Store(&postingList{entries: next, byAddr: byAddr})
	return len(next) == 0
}

// Index 是倒排表。
//
// # 为什么没有锁
//
// 五个维度都拆成「每 key 独立」的原子容器：
//
//   - postings：词 → postingSlot（一槽令牌串行化同词、跨词并行）
//   - docLen / docSize / docTime / docTokens：sync.Map，每文档独立
//   - totalDocs / sumDocLen：atomic
//
// 读者全程只做 Load，永不阻塞写者，写者永不阻塞读者。
//
// 早先这里是一片裸 map，靠调用方（Server）用一把全局 RWMutex 保护。
// 那条纪律漏了两次，第二次是 fatal error: concurrent map iteration
// and map write —— 进程直接没掉，三方瞬间失明。
type Index struct {
	postings  sync.Map // Token -> *postingSlot
	docLen    sync.Map // addr -> int
	docSize   sync.Map // addr -> int
	docTime   sync.Map // addr -> int64
	docTokens sync.Map // addr -> []Token
	// totalDocs 是块总数；sumDocLen 是长度总和（avgLen 的分子）。
	// 缓存 sumDocLen 是为了让 Search 不必遍历全部文档算平均长度。
	totalDocs atomic.Int64
	sumDocLen atomic.Int64
}

// New 创建一个空索引。
func New() *Index { return &Index{} }

// docLenOf 读某篇的词数；不存在返回 0。
func (ix *Index) docLenOf(addr string) int {
	if v, ok := ix.docLen.Load(addr); ok {
		n, _ := v.(int)
		return n
	}
	return 0
}

func (ix *Index) docTimeOf(addr string) int64 {
	if v, ok := ix.docTime.Load(addr); ok {
		t, _ := v.(int64)
		return t
	}
	return 0
}

func (ix *Index) docTokensOf(addr string) []Token {
	if v, ok := ix.docTokens.Load(addr); ok {
		t, _ := v.([]Token)
		return t
	}
	return nil
}

// slot 取（或建）一个词的写槽。
func (ix *Index) slot(tok Token) *postingSlot {
	if v, ok := ix.postings.Load(tok); ok {
		ps, _ := v.(*postingSlot)
		return ps
	}
	ps := newPostingSlot()
	v, loaded := ix.postings.LoadOrStore(tok, ps)
	if loaded {
		old, _ := v.(*postingSlot)
		return old
	}
	return ps
}

// RawView 是索引的一次性只读视图，用于落盘与全量导出。
//
// 它把内部的无锁结构一次性摊平成普通 map/slice —— 这些操作只在
// 重建或落盘时发生，此时天然没有并发写，所以摊平是安全的。
type RawView struct {
	Postings  map[Token]map[string]int
	DocLen    map[string]int
	DocSize   map[string]int
	DocTime   map[string]int64
	DocTokens map[string][]Token
	TotalDocs int
}

// View 摊平出一份只读视图。
func (ix *Index) View() *RawView {
	v := &RawView{
		Postings:  make(map[Token]map[string]int),
		DocLen:    make(map[string]int),
		DocSize:   make(map[string]int),
		DocTime:   make(map[string]int64),
		DocTokens: make(map[string][]Token),
		TotalDocs: int(ix.totalDocs.Load()),
	}
	ix.docLen.Range(func(k, val any) bool {
		n, _ := val.(int)
		v.DocLen[k.(string)] = n
		return true
	})
	ix.docSize.Range(func(k, val any) bool {
		n, _ := val.(int)
		v.DocSize[k.(string)] = n
		return true
	})
	ix.docTime.Range(func(k, val any) bool {
		t, _ := val.(int64)
		v.DocTime[k.(string)] = t
		return true
	})
	ix.docTokens.Range(func(k, val any) bool {
		t, _ := val.([]Token)
		v.DocTokens[k.(string)] = t
		return true
	})
	ix.postings.Range(func(k, val any) bool {
		ps, _ := val.(*postingSlot)
		if ps == nil {
			return true
		}
		list := ps.load()
		if len(list.entries) == 0 {
			return true
		}
		m := make(map[string]int, len(list.entries))
		for _, e := range list.entries {
			m[e.Addr] = e.TF
		}
		v.Postings[k.(Token)] = m
		return true
	})
	return v
}

// Add 把一个块加入索引，时间戳默认为 0。
func (ix *Index) Add(addr string, text string) {
	ix.AddWithTime(addr, text, 0)
}

// AddWithTime 把一个块连同其修改时间加入索引。
//
// 已存在的地址会被**替换**（先撤旧再写新），否则旧词条会残留，
// 导致「内容改了但还能被旧词搜到」。
func (ix *Index) AddWithTime(addr string, text string, modTime int64) {
	ix.Remove(addr)

	tokens := Tokenize(text)
	if len(tokens) == 0 {
		ix.docTime.Store(addr, modTime)
		return
	}
	ix.docTime.Store(addr, modTime)
	ix.docLen.Store(addr, len(tokens))
	ix.docSize.Store(addr, len(text))
	ix.docTokens.Store(addr, tokens)
	ix.totalDocs.Add(1)
	ix.sumDocLen.Add(int64(len(tokens)))

	// 同一个词在本文里出现多次 → 词频累加。
	freq := make(map[Token]int, len(tokens))
	for _, tok := range tokens {
		freq[tok]++
	}
	for tok, tf := range freq {
		ix.slot(tok).add(addr, tf)
	}
}

// Remove 从索引里精确撤销一个块。
//
// 返回是否真的删掉了（不存在时返回 false，不报错——调用方通常在
// 「把旧树换掉」的循环里调它，重复删除是正常的）。
func (ix *Index) Remove(addr string) bool {
	tokens := ix.docTokensOf(addr)
	if tokens == nil {
		// 没有 token 记录但可能有长度/时间记录（空正文的块）
		if _, had := ix.docTime.Load(addr); had {
			ix.docTime.Delete(addr)
			ix.docLen.Delete(addr)
			ix.docSize.Delete(addr)
			return true
		}
		return false
	}
	// 词频去重后逐个撤：同一个词撤一次即可（posting 里一条记录带 tf）。
	seen := make(map[Token]struct{}, len(tokens))
	for _, tok := range tokens {
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		if empty := ix.slot(tok).remove(addr); empty {
			// 词表里清掉空槽，避免 postings 无限膨胀。
			if v, ok := ix.postings.Load(tok); ok {
				ps, _ := v.(*postingSlot)
				if ps != nil && len(ps.load().entries) == 0 {
					ix.postings.CompareAndDelete(tok, ps)
				}
			}
		}
	}
	if n := ix.docLenOf(addr); n > 0 {
		ix.sumDocLen.Add(-int64(n))
	}
	ix.docTokens.Delete(addr)
	ix.docLen.Delete(addr)
	ix.docSize.Delete(addr)
	ix.docTime.Delete(addr)
	ix.totalDocs.Add(-1)
	return true
}

// RemoveBatch 批量撤销一批文档，语义与逐条 Remove 完全一致。
//
// # 为什么必须有它
//
// 逐条 Remove 是 O(N²)：entries 是不可变切片，每撇一个 (doc, token)
// 对都要整份复制一遍。而「重建一条分支的索引」意味着先把整条分支的
// 两万篇全撤掉——它们共同命中同一批常见词，于是每次复制都是一万
// 多条记录。实测 20,448 篇逐条 Remove 要 4m33s，而分词只要 1~2 秒。
//
// 这里把「要撤的 addr」按词聚合，每个词只复制一次。
func (ix *Index) RemoveBatch(addrs []string) {
	if len(addrs) == 0 {
		return
	}

	// ① 先把每个 addr 的 token 记下来，按词倒排：token -> 待撤的 addr 集。
	byToken := make(map[Token]map[string]bool, 4096)
	docs := 0
	var sumLen int64
	for _, addr := range addrs {
		tokens := ix.docTokensOf(addr)
		if tokens == nil {
			// 没有 token 记录但可能有长度/时间记录（空正文的块）
			ix.docTime.Delete(addr)
			ix.docLen.Delete(addr)
			ix.docSize.Delete(addr)
			continue
		}
		// 同一个词在同一篇里出现多次，撤一次就够（posting 里带 tf）。
		seen := make(map[Token]struct{}, len(tokens))
		for _, tok := range tokens {
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			set := byToken[tok]
			if set == nil {
				set = make(map[string]bool, 1)
				byToken[tok] = set
			}
			set[addr] = true
		}
		sumLen += int64(ix.docLenOf(addr))
		ix.docTokens.Delete(addr)
		ix.docLen.Delete(addr)
		ix.docSize.Delete(addr)
		ix.docTime.Delete(addr)
		docs++
	}
	if docs > 0 {
		ix.totalDocs.Add(-int64(docs))
	}
	if sumLen > 0 {
		ix.sumDocLen.Add(-sumLen)
	}

	// ② 每个词只复制、只写回一次。
	for tok, gone := range byToken {
		if empty := ix.slot(tok).removeMany(gone); empty {
			if v, ok := ix.postings.Load(tok); ok {
				ps, _ := v.(*postingSlot)
				if ps != nil && len(ps.load().entries) == 0 {
					ix.postings.CompareAndDelete(tok, ps)
				}
			}
		}
	}
}

// Hit 是一条检索结果。
type Hit struct {
	Addr  string
	Score float64
	// Matched 是命中的词，按贡献降序，用于解释「为什么它被选中」。
	Matched []Token
}

// Search 返回与 query 最相关的块。
//
// 打分用 BM25 的骨架，但做了两处针对记忆场景的调整：
//
//  1. IDF 有下限。在几十个块的小语料里，一个词若出现在所有块中，
//     IDF 会趋近 0；若只出现在一个块中，IDF 会很大。后者会让某个块
//     靠一个罕见词霸占全部结果。用 idfFloor 压住上限。
//  2. 词频饱和。同一个词出现十次不该比出现两次强五倍——记忆条目本来
//     就短，重复通常是措辞而非主题。
//
// WeightFunc 返回一个地址的被读频次。nil 表示不加权。
// FilterFunc 判断某个地址是否参与检索。空表示不限制。
type FilterFunc func(addr string) bool

type WeightFunc func(addr string) int

func (ix *Index) Search(query string, limit int, idfFloor float64) []Hit {
	return ix.SearchWeighted(query, limit, idfFloor, nil)
}

// SearchWeighted 是未过滤检索的便捷包装。
func (ix *Index) SearchWeighted(query string, limit int, idfFloor float64, wf WeightFunc) []Hit {
	return ix.SearchWeightedFiltered(query, limit, idfFloor, wf, nil)
}

// SearchWeightedFiltered 按照 filter 预筛选候选集合，再做 BM25 打分与排序。
//
// 为什么必须在打分前过滤，而不是打完分之后再抛弃：
// 当索引包含多个分支时，如果不加过滤，全局 top-N
// 很容易全被其他分支的同名词占满——随后的权限过滤把它们全部拦下，
// 最终返回「无命中」。
//
// 过滤是安全边界，也是检索相关性的边界：在这张凭证的可见范围里算出来的
// top-N，才是它该看见的最佳答案。
// SearchWeightedFiltered 按权重和外部过滤器打分。
func (ix *Index) SearchWeightedFiltered(query string, limit int, idfFloor float64, wf WeightFunc, filter FilterFunc) []Hit {
	return ix.SearchCovered(query, limit, idfFloor, 0, wf, filter)
}

// SearchCovered 增加了最小查询词覆盖率（minCoverage）。
//
// 为什么需要它：中日韩文字按二元组（bigram）分词，任何较长的中文查询
// 几乎都能撞上一两个常见词（例如「量子计算机退相干」里的「计算」「算机」）。
// BM25 会给这些偶然重合打出大于 0 的分数，导致与事实完全无关的查询
// 在每层都「命中」——这不仅向模型输出无关正文，还让 '||' 回落链彻底
// 失效（因为第一层总是「命中」，后面两层永远不会被执行）。
//
// 覆盖率 = 命中的不同查询词数 / 查询里去重后的总词数。
// 阈值 0.5 意味着至少一半的查询词必须出现在文档里才算命中；
// 0 表示关掉覆盖率检查，保持纯 BM25 行为。
func (ix *Index) SearchCovered(query string, limit int, idfFloor, minCoverage float64, wf WeightFunc, filter FilterFunc) []Hit {
	qtokens := Tokenize(query)
	total := ix.totalDocs.Load()
	if len(qtokens) == 0 || total == 0 {
		return nil
	}

	// 去重，但保留查询里重复词的权重意义有限，这里按集合处理。
	seen := make(map[Token]bool, len(qtokens))
	uniq := make([]Token, 0, len(qtokens))
	for _, t := range qtokens {
		if !seen[t] {
			seen[t] = true
			uniq = append(uniq, t)
		}
	}

	// b 有意取小值。BM25 默认 b=0.75 会重罚长文档，但在记忆场景里
	// 「更长」通常意味着「包含更多说明」，尤其订正页总是比被订正的旧页长。
	// 用 b=0.3 保留一点长度影响，但不让它压过内容相关度。
	const (
		k1 = 1.2
		b  = 0.3
	)

	// avgLen 用缓存的 sumDocLen 直接算，不必遍历全部文档——
	// 那曾经是每次检索都要扫两万次 map 的开销。
	avgLen := 1.0
	if total > 0 {
		avgLen = float64(ix.sumDocLen.Load()) / float64(total)
	}
	if avgLen == 0 {
		avgLen = 1
	}

	scores := make(map[string]float64)
	matched := make(map[string][]Token)

	for _, tok := range uniq {
		v, ok := ix.postings.Load(tok)
		if !ok {
			continue
		}
		ps, _ := v.(*postingSlot)
		if ps == nil {
			continue
		}
		entries := ps.load().entries
		if len(entries) == 0 {
			continue
		}
		df := len(entries)
		idf := math.Log(1 + (float64(total)-float64(df)+0.5)/(float64(df)+0.5))
		if idfFloor > 0 && idf > idfFloor {
			idf = idfFloor
		}

		for _, e := range entries {
			if filter != nil && !filter(e.Addr) {
				continue
			}
			dl := float64(ix.docLenOf(e.Addr))
			if dl == 0 {
				dl = 1
			}
			tf := float64(e.TF)
			num := tf * (k1 + 1)
			den := tf + k1*(1-b+b*dl/avgLen)
			scores[e.Addr] += idf * num / den
			matched[e.Addr] = append(matched[e.Addr], tok)
		}
	}

	// 找最大时间戳，作为基准
	maxTime := int64(0)
	docTimes := make(map[string]int64, len(scores))
	for addr := range scores {
		t := ix.docTimeOf(addr)
		docTimes[addr] = t
		if t > maxTime {
			maxTime = t
		}
	}

	hits := make([]Hit, 0, len(scores))
	totalQueryTerms := float64(len(uniq))
	for addr, s := range scores {
		if minCoverage > 0 && totalQueryTerms > 0 {
			mTokens := matched[addr]
			seenM := make(map[Token]bool, len(mTokens))
			for _, t := range mTokens {
				seenM[t] = true
			}
			coverage := float64(len(seenM)) / totalQueryTerms
			if coverage < minCoverage {
				continue
			}
		}

		// 时间增益：距离最新条目越近，给 1.0 ~ 1.25x 的乘数
		boost := 1.0
		if maxTime > 0 && docTimes[addr] > 0 {
			ageSec := float64(maxTime - docTimes[addr])
			if ageSec >= 0 {
				// 30 天半衰到 1.1x，当天 1.25x
				boost = 1.0 + 0.60/(1.0+ageSec/(30*86400))
			}
		}
		freqBoost := 1.0
		if wf != nil {
			// 对数增长：1 次 1.10x，5 次 1.27x，10 次 1.36x，30 次 1.51x。
			// 上限 1.6x：绝不能盖过 BM25 本身的判定，重读多只在相关度相近时决胜。
			c := wf(addr)
			if c > 0 {
				freqBoost = 1.0 + 0.15*math.Log1p(float64(c))
				if freqBoost > 1.6 {
					freqBoost = 1.6
				}
			}
		}
		hits = append(hits, Hit{Addr: addr, Score: s * boost * freqBoost, Matched: matched[addr]})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Addr < hits[j].Addr // 稳定排序，结果可复现
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// Docs 返回索引里的块数。
func (ix *Index) Docs() int { return int(ix.totalDocs.Load()) }

// Terms 返回索引里的词数。
func (ix *Index) Terms() int {
	n := 0
	ix.postings.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// DocTime 查出一个块的逻辑时间（Unix 秒）。未知返回 0。
//
// 供 log 排序与时效衰减复用：避免 log 每次执行都把全库切片
// 从 git 裸库里解包重读一次 frontmatter。
func (ix *Index) DocTime(addr string) int64 {
	if ix == nil {
		return 0
	}
	return ix.docTimeOf(addr)
}

// Tokenize 把文本切成索引词。
//
// 规则：
//   - 连续的中日韩文字按二元组切分
//   - 连续的字母数字按整词切分并小写
//   - 其他字符（标点、空白、符号）作为分隔
//
// 单字中文不单独成词：单字在中文里太泛，检索价值低于噪音成本。
func Tokenize(text string) []Token {
	var out []Token
	var han []rune
	var word []rune

	flushHan := func() {
		if len(han) >= 2 {
			for i := 0; i+1 < len(han); i++ {
				out = append(out, Token(string(han[i:i+2])))
			}
		} else if len(han) == 1 {
			out = append(out, Token(string(han)))
		}
		han = han[:0]
	}
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, Token(strings.ToLower(string(word))))
			word = word[:0]
		}
	}

	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushHan()
			flushWord()
		}
	}
	flushHan()
	flushWord()
	return out
}

// BatchDoc 是一条待批量索引的文档。
type BatchDoc struct {
	Addr    string
	Text    string
	ModTime int64
}

// AddBatch 并行分词、分段写索引。
//
// # 为什么需要它
//
// AddWithTime 是纯串行的：分词是 CPU 密集（Unicode 查表、中文二元切分、
// 词频统计），而主循环一次只喂一篇。实测冷建 5,000 篇耗时 6.1 秒，
// 其中 3.5 秒（57%）都在这条串行路径上——多核机器上只跑满一个核。
//
// # 为什么可以安全并行
//
// postingSlot 本来就是按词分段的（每词一槽令牌 + 原子发布不可变切片），
// 设计目的就是让「写罕见词」不必等「写常见词」。于是分词可以在 N 个
// goroutine 里同时进行，只在最后按词写入时短暂串行——而不同词之间
// 互不阻塞，所以总吞吐随核数上升。
//
// # 冷建为什么能省掉 Remove
//
// AddWithTime 每篇都先 Remove(addr) 再写，是为了「内容改了但旧词还能
// 搜到」这个正确性要求。冷建时索引是全新的，Remove 必然空转
// （查 docTokens、查 docTime 全部 miss）。批量路径直接跳过它。
func (ix *Index) AddBatch(docs []BatchDoc, workers int) {
	if len(docs) == 0 {
		return
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(docs) {
		workers = len(docs)
	}

	type tokenized struct {
		idx     int
		tokens  []Token
		size    int
		skipped bool
	}
	out := make([]tokenized, len(docs))

	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				toks := Tokenize(docs[i].Text)
				out[i] = tokenized{idx: i, tokens: toks, size: len(docs[i].Text), skipped: len(toks) == 0}
			}
		}()
	}
	for i := range docs {
		work <- i
	}
	close(work)
	wg.Wait()

	// 写入阶段：
	//
	// 如果索引不是空的（增量更新），先撤旧条目，防止「内容改了但旧词仍能搜到」
	// （见 TestAddBatchOverwriteSameAddr 钉住的行为）。
	// 冷建时 totalDocs==0，这一步直接跳过，零开销。
	//
	// 走 RemoveBatch 而不是逐条 Remove：批量版按词聚合，每个词只复制
	// 一次倒排表。逐条版在「整条分支重建」这个场景下是 O(N²)——
	// 20,448 篇实测 4m33s。
	if ix.Docs() > 0 {
		addrs := make([]string, len(docs))
		for i := range docs {
			addrs[i] = docs[i].Addr
		}
		ix.RemoveBatch(addrs)
	}
	for i := range out {
		d := docs[i]
		ix.docTime.Store(d.Addr, d.ModTime)
		if out[i].skipped {
			continue
		}
		ix.docLen.Store(d.Addr, len(out[i].tokens))
		ix.docSize.Store(d.Addr, out[i].size)
		ix.docTokens.Store(d.Addr, out[i].tokens)
		ix.totalDocs.Add(1)
		ix.sumDocLen.Add(int64(len(out[i].tokens)))
	}

	// 按词聚合，每词只写一次（addMany）。
	//
	// 逐条 add 是 O(N²)：entries 是不可变切片，每次都要整份复制。
	// 5,000 篇共享同一批常见词时，一个中文二元组的倒排累计复制上千万条
	// 记录——实测这一步 7.04 秒，而分词只有 20 毫秒。
	byToken := make(map[Token][]postingEntry, 4096)
	for i := range out {
		if out[i].skipped {
			continue
		}
		addr := docs[i].Addr
		freq := make(map[Token]int, len(out[i].tokens))
		for _, tok := range out[i].tokens {
			freq[tok]++
		}
		for tok, tf := range freq {
			byToken[tok] = append(byToken[tok], postingEntry{Addr: addr, TF: tf})
		}
	}
	for tok, pairs := range byToken {
		ix.slot(tok).addMany(pairs)
	}
}
