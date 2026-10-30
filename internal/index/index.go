// Package index 把可索引的块变成倒排表。
//
// 只做一件事：词 → 块。没有语义、没有模型、没有网络。
// 复杂度全部花在「哪些词值得记」上，因为噪音词会让联想变成随机数。
package index

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Token 是一个索引词。
//
// 中文按二元组切分（"记忆服务" → 记忆/忆服/服务），英文按词切分并小写化。
// 不做词性标注、不做同义词扩展——那需要模型，而这一层刻意不用模型。
type Token string

// Index 是倒排表。
type Index struct {
	// postings 词 → 块地址 → 词频
	postings map[Token]map[string]int
	// docLen 块地址 → 词数
	docLen map[string]int
	// docSize 块地址 → 字节数，用于长度归一化
	docSize map[string]int
	// docTime 块地址 → 修改时间（Unix 秒），0 表示未知
	docTime map[string]int64
	// totalDocs 块总数
	totalDocs int
}

// New 创建一个空索引。
func New() *Index {
	return &Index{
		postings: make(map[Token]map[string]int),
		docLen:   make(map[string]int),
		docSize:  make(map[string]int),
		docTime:  make(map[string]int64),
	}
}

// Add 把一个块加入索引，时间戳默认为 0。
func (ix *Index) Add(addr string, text string) {
	ix.AddWithTime(addr, text, 0)
}

// AddWithTime 把一个块连同其修改时间加入索引。
func (ix *Index) AddWithTime(addr string, text string, modTime int64) {
	ix.docTime[addr] = modTime
	tokens := Tokenize(text)
	if len(tokens) == 0 {
		return
	}
	ix.docLen[addr] = len(tokens)
	ix.docSize[addr] = len(text)
	ix.totalDocs++

	for _, tok := range tokens {
		m, ok := ix.postings[tok]
		if !ok {
			m = make(map[string]int)
			ix.postings[tok] = m
		}
		m[addr]++
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
// 当索引包含多个分支（zero/chat/dev）时，如果不加过滤，全局 top-N
// 很容易全被其他分支的同名词占满——随后的权限过滤把它们全部拦下，
// 最终返回「无命中」。
//
// 过滤是安全边界，也是检索相关性的边界：在这个角色的世界里算出来的
// top-N，才是它该看见的最佳答案。
func (ix *Index) SearchWeightedFiltered(query string, limit int, idfFloor float64, wf WeightFunc, filter FilterFunc) []Hit {
	qtokens := Tokenize(query)
	if len(qtokens) == 0 || ix.totalDocs == 0 {
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

	avgLen := 0.0
	for _, n := range ix.docLen {
		avgLen += float64(n)
	}
	if ix.totalDocs > 0 {
		avgLen /= float64(ix.totalDocs)
	}
	if avgLen == 0 {
		avgLen = 1
	}

	scores := make(map[string]float64)
	matched := make(map[string][]Token)

	for _, tok := range uniq {
		posting, ok := ix.postings[tok]
		if !ok {
			continue
		}
		df := len(posting)
		idf := math.Log(1 + (float64(ix.totalDocs)-float64(df)+0.5)/(float64(df)+0.5))
		if idfFloor > 0 && idf > idfFloor {
			idf = idfFloor
		}

		for addr, tf := range posting {
			if filter != nil && !filter(addr) {
				continue
			}
			dl := float64(ix.docLen[addr])
			if dl == 0 {
				dl = 1
			}
			num := float64(tf) * (k1 + 1)
			den := float64(tf) + k1*(1-b+b*dl/avgLen)
			scores[addr] += idf * num / den
			matched[addr] = append(matched[addr], tok)
		}
	}

	// 找最大时间戳，作为基准
	var maxTime int64
	for addr := range scores {
		if t := ix.docTime[addr]; t > maxTime {
			maxTime = t
		}
	}

	hits := make([]Hit, 0, len(scores))
	for addr, s := range scores {
		// 时间增益：距离最新条目越近，给 1.0 ~ 1.25x 的乘数
		boost := 1.0
		if maxTime > 0 && ix.docTime[addr] > 0 {
			ageSec := float64(maxTime - ix.docTime[addr])
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
func (ix *Index) Docs() int { return ix.totalDocs }

// Terms 返回索引里的词数。
func (ix *Index) Terms() int { return len(ix.postings) }

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
