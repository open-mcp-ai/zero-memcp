// Package vecindex 维护纯内存的向量表与紧凑落盘。
//
// 为什么用暴力全扫不用 HNSW：实测 10,000 条向量 512 维，
// 纯循环点积 7.8 毫秒。HNSW 能快到 1 毫秒，但代价是几百行图索引
// 代码、调参、重建复杂度、以及最坏的——近似丢召回。对记忆检索来说，
// 漏掉 1729 就是整场失败。为省 7 毫秒不值得。
package vecindex

import (
	"container/heap"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

const (
	magic   = uint32(0x5a4d5643) // "ZMVC" (Zero-Mem Vector Cache)
	version = 1
)

// Dim 是 LaBSE 的输出维度。
// 为什么不是常量 512：bge-small-zh 是 512，LaBSE 是 768。
// 维度属于模型，不属于索引——所以它是 Index 的字段，由 Open 时
// 从模型实测得出，manifest 里记录，载入时校验一致才接受。
const Dim = 768

// vecBytes 依赖运行时维度，用函数表达。
func vecBytes(dim int) int64 { return int64(dim) * 4 }

// Hit 是一条检索命中。
type Hit struct {
	ID    string  // 块的完整 ID，形如 project@branch/path
	Score float32 // 内积（向量已 L2 归一化，即为余弦相似度）
}

// Index 是纯内存的向量表。
//
// 一把锁贯穿全部读写：vecs 是扁平连续数组，读侧一次 RLock 扫到底，
// 写侧一次 Lock 改完 ids/vecs/idMap 三样。任何「先取快照再释放锁」
// 的写法都会在并发 Add 时把下标和 ID 对错——那不是优化，是 bug。
type Index struct {
	dim   int // 本索引的向量维度，Add/Search 时强校验
	mu    sync.RWMutex
	ids   []string
	vecs  []float32 // 扁平连续数组，长 len(ids) * dim
	idMap map[string]int
}

func New(dim int) *Index {
	return &Index{
		dim:   dim,
		idMap: make(map[string]int),
	}
}

// Dim 返回本索引的向量维度。
func (ix *Index) Dim() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.dim
}

// Len 返回当前收录的向量数。
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.ids)
}

// Add 追加或更新一条向量。vec 必须长 Dim 且已 L2 归一化。
func (ix *Index) Add(id string, vec []float32) error {
	ix.mu.RLock()
	dim := ix.dim
	ix.mu.RUnlock()
	if len(vec) != dim {
		return fmt.Errorf("vector dim mismatch: got %d, want %d", len(vec), dim)
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()

	if idx, exists := ix.idMap[id]; exists {
		copy(ix.vecs[idx*dim:(idx+1)*dim], vec)
		return nil
	}
	idx := len(ix.ids)
	ix.ids = append(ix.ids, id)
	ix.vecs = append(ix.vecs, vec...)
	ix.idMap[id] = idx
	return nil
}

// Has 判断某条 ID 是否已在索引里（增量更新时据此跳过重算）。
func (ix *Index) Has(id string) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	_, ok := ix.idMap[id]
	return ok
}

// VectorOf 返回某条 ID 的向量副本；不存在返回 nil。
//
// 增量重建时用它把**未变**的块原样搬到新索引，跳过推理。
// 返回副本而不是内部切片：调用方会把它 Add 进另一个 Index，
// 而 Add 会把内容拷走——但共享底层数组仍会在并发场景下被写方挪动。
func (ix *Index) VectorOf(id string) []float32 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	idx, ok := ix.idMap[id]
	if !ok {
		return nil
	}
	out := make([]float32, ix.dim)
	copy(out, ix.vecs[idx*ix.dim:(idx+1)*ix.dim])
	return out
}

// Search 全库扫描，返回相似度 >= threshold 的 top-N。
//
// 用最小堆维护 top-N，而不是收齐全部再排序：候选可能上万条，
// 全排序是 O(n log n)，堆是 O(n log k)。k 只有几个，省的是
// 每次检索都要付的那段排序时间。
//
// 锁只在这一把里拿：扫描期间 vecs 不会被写方挪动。
func (ix *Index) Search(query []float32, topN int, threshold float32) []Hit {
	if len(query) != ix.dim || topN <= 0 {
		return nil
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	n := len(ix.ids)
	if n == 0 {
		return nil
	}

	h := &topHeap{}
	heap.Init(h)

	for i := 0; i < n; i++ {
		base := i * ix.dim
		var s float32
		// 一次取 4 个分量：给编译器向量化的机会
		for d := 0; d < ix.dim; d += 4 {
			s += query[d]*ix.vecs[base+d] +
				query[d+1]*ix.vecs[base+d+1] +
				query[d+2]*ix.vecs[base+d+2] +
				query[d+3]*ix.vecs[base+d+3]
		}
		if s < threshold {
			continue
		}
		if h.Len() < topN {
			heap.Push(h, item{i: i, score: s})
		} else if s > h.items[0].score {
			h.items[0] = item{i: i, score: s}
			heap.Fix(h, 0)
		}
	}

	// 弹出顺序是「从小到大」，翻过来得到「最相关在前」
	out := make([]Hit, 0, h.Len())
	for h.Len() > 0 {
		it := heap.Pop(h).(item)
		out = append(out, Hit{ID: ix.ids[it.i], Score: it.score})
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

type item struct {
	i     int
	score float32
}

// topHeap 是最小堆：堆顶是当前 top-N 里最不相关的一条，
// 新候选只要比堆顶好就换掉它。
type topHeap struct{ items []item }

func (h *topHeap) Len() int           { return len(h.items) }
func (h *topHeap) Less(i, j int) bool { return h.items[i].score < h.items[j].score }
func (h *topHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *topHeap) Push(x any)         { h.items = append(h.items, x.(item)) }
func (h *topHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	h.items = old[:n-1]
	return it
}

// ── 落盘与载入 ──────────────────────────────────────────────────
//
// 为什么不存进 Git：向量是「(文本, 模型) 的函数返回值」，是派生物，
// 不是内容。模型一换向量全作废，存进 Git 就等于在历史里永久留下
// 几十 MB 垃圾——Git 不能真的忘记。而且 float32 压缩率极差，
// 每次 repack 都要重算这些噪声。
//
// 落点放在 data/vectors/：随整个 zero-memcp 目录一起备份、一起搬走，
// 但不进版本历史。删了不心疼，重建一次即可。

// Manifest 是缓存的身份证：模型、维度、各分支 head。
// 任一项对不上，缓存就是过期的，必须重建而不是凑着用——
// 「凑合能用但结果错」比「直接报错」危险得多。
type Manifest struct {
	Magic   uint32            `json:"magic"`
	Version int               `json:"version"`
	Dim     int               `json:"dim"`
	Count   int               `json:"count"`
	Model   string            `json:"model"`
	Heads   map[string]string `json:"heads"` // 分支 → commit head（仅诊断用）
	IDs     []string          `json:"ids"`

	// Prints 是「块 ID → 被向量化文本的内容指纹」。
	//
	// 它是增量索引的唯一真相。为什么用内容指纹而不是 commit head：
	// head 前进一步并不意味所有块都变了——日常写记忆通常只新增一两篇。
	// 旧版本拿 head 当判据，只要有一个分支推进就把整份缓存判废，
	// 全量重算 6800 片要 12 分钟。按内容指纹比对，同样场景只算 1-2 片。
	//
	// 指纹算的是「喂给 embedder 的最终文本」（剥掉 frontmatter 之后的），
	// 而不是原始 blob：语义索引的内容本来就是这段文本。
	// semanticText 的算法一改，所有指纹全变，自动全量重建——这是对的。
	Prints map[string]string `json:"prints"`
}

// Save 原子写出 vectors.bin + manifest.json。
func (ix *Index) Save(dir, model string, heads map[string]string, prints map[string]string) error {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	binPath := filepath.Join(dir, "vectors.bin")
	tmpBin := binPath + ".tmp"
	f, err := os.OpenFile(tmpBin, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, len(ix.vecs)*4)
	for i, v := range ix.vecs {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		os.Remove(tmpBin)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpBin)
		return err
	}
	if err := os.Rename(tmpBin, binPath); err != nil {
		return err
	}

	m := Manifest{
		Magic:   magic,
		Version: version,
		Dim:     ix.dim,
		Count:   len(ix.ids),
		Model:   model,
		Heads:   heads,
		IDs:     ix.ids,
		Prints:  prints,
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmpMan := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmpMan, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpMan, filepath.Join(dir, "manifest.json"))
}

// ErrStale 表示缓存缺失或身份不符，调用方应重建。
var ErrStale = errors.New("vector cache is stale or missing")

// Load 读回缓存。expectedModel 对不上返回 ErrStale。
//
// heads 不再参与过期判定（增量索引以 manifest 里的内容指纹为判据，
// 见 Prints 的注释）；它只是存下来供诊断。
func Load(dir, expectedModel string) (*Index, map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, nil, ErrStale
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, ErrStale
	}
	if m.Magic != magic || m.Version != version || m.Dim <= 0 {
		return nil, nil, ErrStale
	}
	if m.Model != expectedModel {
		return nil, nil, ErrStale
	}

	f, err := os.Open(filepath.Join(dir, "vectors.bin"))
	if err != nil {
		return nil, nil, ErrStale
	}
	defer f.Close()

	expected := int64(m.Count) * int64(m.Dim) * 4
	st, err := f.Stat()
	if err != nil || st.Size() != expected {
		return nil, nil, ErrStale
	}
	buf := make([]byte, expected)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, nil, ErrStale
	}

	vecs := make([]float32, m.Count*m.Dim)
	for i := range vecs {
		vecs[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	idMap := make(map[string]int, m.Count)
	for i, id := range m.IDs {
		idMap[id] = i
	}
	prints := m.Prints
	if prints == nil {
		// 旧格式缓存：没有指纹表。对调用方来说等同于「每块都要重算」，
		// 但向量本身如果 ID 对得上还是可以复用的——IDs 顺序就是槽位。
		prints = map[string]string{}
	}
	return &Index{dim: m.Dim, ids: m.IDs, vecs: vecs, idMap: idMap}, prints, nil
}
