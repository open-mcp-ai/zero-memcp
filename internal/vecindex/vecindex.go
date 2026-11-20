// Package vecindex 维护纯内存的向量表与紧凑落盘。
//
// 为什么用暴力全扫不用 HNSW：实测 10,000 条向量 512 维，
// 纯循环点积 7.8 毫秒。HNSW 能快到 1 毫秒，但代价是几百行图索引
// 代码、调参、重建复杂度、以及最坏的——近似丢召回。对记忆检索来说，
// 漏掉 1729 就是整场失败。为省 7 毫秒不值得。
package vecindex

import (
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
// vecBlock 是一块向量存储。
//
// 一块之内 ids/vecs 连续存放，保留扁平数组的缓存局部性——
// Search 是逐条点积，跨块跳转只在块边界发生（21827 条 / 512 = 43 次）。
type vecBlock struct {
	ids  []string
	vecs []float32 // 长 n*dim
	n    int
}

// vecBlockCap 是每块容纳的向量数。取 512 是因为它同时满足两件事：
// 复制一块的代价小（512 × 1024 × 4B = 2MB），块数也少（43 块）。
const vecBlockCap = 512

// Index 是纯内存的向量表。
//
// # 为什么没有锁
//
// 三条发布纪律，合起来保证读者永远看到一致状态：
//
//  1. blocks 用 atomic.Pointer 发布**不可变**的块列表。列表本身很短，
//     复制它是微秒级。
//
//  2. count 用 atomic.Int64 发布「有效条数」。追加时，写手先往
//     下标 >= count 的槽位写数据，**再** count.Store(n+1)。
//     读者只读 [0, count)，永远碰不到正在被写的那一槽；
//     原子写的 happens-before 保证读者能看到数据。
//
//  3. 更新**已存在**的向量（同 id 重算）不能就地改——那块可能正被
//     Search 扫描。此时复制整块、改副本、换进块列表。
//     这只发生在「文档内容变了要重算」，是低频动作。
//
// 早先这里是一把 RWMutex 包住 ids/vecs/idMap 三样：Search 与 Add
// 互相挡路，而 Add 的调用方（语义索引重建）还可能在做网络推理。
type Index struct {
	dim int // 本索引的向量维度，Add/Search 时强校验
	// blocks 是不可变的块列表快照。
	blocks atomic.Pointer[[]*vecBlock]
	// count 是已发布的有效条数。
	count atomic.Int64
	// idMap 是 id -> 全局下标。每 key 独立，用 sync.Map。
	idMap sync.Map
}

func New(dim int) *Index {
	ix := &Index{dim: dim}
	empty := []*vecBlock{}
	ix.blocks.Store(&empty)
	return ix
}

// Dim 返回本索引的向量维度。
func (ix *Index) Dim() int { return ix.dim }

// Len 返回当前收录的向量数。
func (ix *Index) Len() int { return int(ix.count.Load()) }

func (ix *Index) loadBlocks() []*vecBlock {
	if p := ix.blocks.Load(); p != nil {
		return *p
	}
	return nil
}

func (ix *Index) storeBlocks(b []*vecBlock) { ix.blocks.Store(&b) }

// locate 把全局下标换算成 (块号, 块内偏移)。
func locate(idx int) (int, int) { return idx / vecBlockCap, idx % vecBlockCap }

// Add 追加或更新一条向量。vec 必须长 Dim 且已 L2 归一化。
func (ix *Index) Add(id string, vec []float32) error {
	dim := ix.dim
	if len(vec) != dim {
		return fmt.Errorf("vector dim mismatch: got %d, want %d", len(vec), dim)
	}

	// 已存在 → 更新。复制那一块、改副本、换进块列表。
	if v, ok := ix.idMap.Load(id); ok {
		idx, _ := v.(int)
		bi, si := locate(idx)
		blocks := ix.loadBlocks()
		if bi < len(blocks) {
			old := blocks[bi]
			clone := &vecBlock{
				ids:  make([]string, len(old.ids)),
				vecs: make([]float32, len(old.vecs)),
				n:    old.n,
			}
			copy(clone.ids, old.ids)
			copy(clone.vecs, old.vecs)
			if si < clone.n && (si+1)*dim <= len(clone.vecs) {
				copy(clone.vecs[si*dim:(si+1)*dim], vec)
				next := make([]*vecBlock, len(blocks))
				copy(next, blocks)
				next[bi] = clone
				ix.storeBlocks(next)
			}
		}
		return nil
	}

	// 新增：在 count 之外写数据，最后发布 count。
	n := int(ix.count.Load())
	bi, si := locate(n)
	blocks := ix.loadBlocks()

	block := &vecBlock{}
	if bi < len(blocks) && blocks[bi].n < vecBlockCap {
		block = blocks[bi]
	} else {
		// 需要新块：复制块列表并追加。新块此刻无人可见。
		block = &vecBlock{}
		next := make([]*vecBlock, len(blocks), len(blocks)+1)
		copy(next, blocks)
		next = append(next, block)
		ix.storeBlocks(next)
	}

	for len(block.ids) <= si {
		block.ids = append(block.ids, "")
	}
	for len(block.vecs) < (si+1)*dim {
		block.vecs = append(block.vecs, 0)
	}
	block.ids[si] = id
	copy(block.vecs[si*dim:(si+1)*dim], vec)
	if si+1 > block.n {
		block.n = si + 1
	}
	ix.idMap.Store(id, n)
	ix.count.Store(int64(n + 1))
	return nil
}

// Has 判断某条 ID 是否已在索引里（增量更新时据此跳过重算）。
func (ix *Index) Has(id string) bool {
	_, ok := ix.idMap.Load(id)
	return ok
}

// VectorOf 返回某条 ID 的向量副本；不存在返回 nil。
//
// 返回副本而不是内部切片：那块之后可能被复制替换。
func (ix *Index) VectorOf(id string) []float32 {
	v, ok := ix.idMap.Load(id)
	if !ok {
		return nil
	}
	idx, _ := v.(int)
	bi, si := locate(idx)
	blocks := ix.loadBlocks()
	if bi >= len(blocks) {
		return nil
	}
	blk := blocks[bi]
	if (si+1)*ix.dim > len(blk.vecs) {
		return nil
	}
	return append([]float32(nil), blk.vecs[si*ix.dim:(si+1)*ix.dim]...)
}

// Search 全库扫描，返回相似度 >= threshold 的 top-N。
//
// 用最小堆维护 top-N：候选可能上万条，全排序 O(n log n)，
// 堆是 O(n log k)。k 只有几个。
func (ix *Index) Search(query []float32, topN int, threshold float32) []Hit {
	if len(query) != ix.dim || topN <= 0 {
		return nil
	}
	n := int(ix.count.Load())
	if n == 0 {
		return nil
	}
	// 取一次块列表快照：扫描期间块列表可能被换掉，
	// 但快照里的块在被替换前不会被就地修改。
	blocks := ix.loadBlocks()
	dim := ix.dim

	h := &topHeap{}
	heap.Init(h)

	for i := 0; i < n; i++ {
		bi, si := locate(i)
		if bi >= len(blocks) {
			break
		}
		base := si * dim
		if base+dim > len(blocks[bi].vecs) {
			continue
		}
		vecs := blocks[bi].vecs
		var s float32
		for d := 0; d < dim; d += 4 {
			s += query[d]*vecs[base+d] +
				query[d+1]*vecs[base+d+1] +
				query[d+2]*vecs[base+d+2] +
				query[d+3]*vecs[base+d+3]
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

	out := make([]Hit, 0, h.Len())
	for h.Len() > 0 {
		it := heap.Pop(h).(item)
		bi, si := locate(it.i)
		if bi < len(blocks) && si < len(blocks[bi].ids) {
			out = append(out, Hit{ID: blocks[bi].ids[si], Score: it.score})
		}
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
	Magic   uint32 `json:"magic"`
	Version int    `json:"version"`
	Dim     int    `json:"dim"`
	Count   int    `json:"count"`
	Model   string `json:"model"`
	// ModelRev 是**同一模型名下的修订号**（权重文件哈希、后端版本等）。
	//
	// 为什么光有 Model 不够：换了一份重新量化、或者升级了推理后端之后，
	// 模型名字往往一个字都没变，但向量空间已经不同。此时若只比 Model，
	// 新旧向量会被当成同一个空间混在索引里——不报错、不重建，
	// 相似度从此静默失真。
	//
	// 空值表示「未声明修订号」，与任何非空值都不相等，因此从旧版本升级
	// 上来会触发一次重建。这是有意的：宁可多算一次，不混两个空间。
	ModelRev string `json:"model_rev,omitempty"`
	// ModelKey 是落盘子目录名。它在 Save 时由 Model+ModelRev 推导并记下，
	// 使得「磁盘上这个目录是谁的」不必依赖外部约定。
	ModelKey string            `json:"model_key,omitempty"`
	Heads    map[string]string `json:"heads"` // 分支 → commit head（仅诊断用）
	IDs      []string          `json:"ids"`

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

// ModelKeyFor 由模型名与修订号推导一个安全的目录名。
//
// 多个模型（含同名的不同修订）各自独占一个子目录，互不覆盖——
// 这正是「远端大模型挂了切本地小模型、恢复后切回来」不重算的前提。
//
// 规则只保留 [A-Za-z0-9._-]，其余替换成 '-'，并追加修订号的短哈希：
// 目录名必须可读（人要看），又必须唯一（不同修订不能撞）。
func ModelKeyFor(model, rev string) string {
	safe := make([]rune, 0, len(model))
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			safe = append(safe, r)
		default:
			safe = append(safe, '-')
		}
	}
	key := strings.Trim(string(safe), "-")
	if key == "" {
		key = "model"
	}
	if len(key) > 48 {
		key = key[:48]
	}
	if rev == "" {
		return key
	}
	sum := sha256.Sum256([]byte(rev))
	return key + "-" + hex.EncodeToString(sum[:4])
}

// ResolveDir 返回该模型应该使用的落盘目录。
//
// base 是 [semantic] vectors 声明的根目录。多模型共存靠这一层子目录：
//
//	<base>/bge-m3/              ← 远端大模型
//	<base>/bge-small-zh-int8/   ← 本地降级模型
//
// 旧版本把 vectors.bin 直接写在 base 下。若那里恰好是本模型且修订号
// 兼容，调用方会继续用它（见 LoadLegacy），因此升级不需要手工搬文件。
func ResolveDir(base, model, rev string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(base, ModelKeyFor(model, rev))
}

// Save 原子写出 vectors.bin + manifest.json。
func (ix *Index) Save(dir, model string, heads map[string]string, prints map[string]string) error {
	return ix.SaveRev(dir, model, "", heads, prints)
}

// SaveRev 是带修订号的 Save。
func (ix *Index) SaveRev(dir, model, rev string, heads map[string]string, prints map[string]string) error {
	// 先取一份快照（块列表 + 有效条数），落盘全程不持任何保护。
	blocks := ix.loadBlocks()
	n := int(ix.count.Load())
	dim := ix.dim
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		bi, si := locate(i)
		if bi >= len(blocks) || si >= len(blocks[bi].ids) {
			break
		}
		ids = append(ids, blocks[bi].ids[si])
	}
	n = len(ids)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	binPath := filepath.Join(dir, "vectors.bin")
	tmpBin := binPath + ".tmp"
	f, err := os.OpenFile(tmpBin, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, n*dim*4)
	off := 0
	for i := 0; i < n; i++ {
		bi, si := locate(i)
		blk := blocks[bi]
		base := si * dim
		if base+dim > len(blk.vecs) {
			for d := 0; d < dim; d++ {
				binary.LittleEndian.PutUint32(buf[off:], 0)
				off += 4
			}
			continue
		}
		for d := 0; d < dim; d++ {
			binary.LittleEndian.PutUint32(buf[off:], math.Float32bits(blk.vecs[base+d]))
			off += 4
		}
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
		Magic:    magic,
		Version:  version,
		Dim:      dim,
		Count:    n,
		Model:    model,
		ModelRev: rev,
		ModelKey: ModelKeyFor(model, rev),
		Heads:    heads,
		IDs:      ids,
		Prints:   prints,
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
	return LoadRev(dir, expectedModel, "")
}

// LoadRev 在 Load 之上再校验修订号。
//
// 修订号不匹配 == 向量空间可能不同，必须 ErrStale 让调用方重建。
// 这是「同名模型换了权重」这个静默故障的唯一防线。
func LoadRev(dir, expectedModel, expectedRev string) (*Index, map[string]string, error) {
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
	if m.ModelRev != expectedRev {
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

	prints := m.Prints
	if prints == nil {
		// 旧格式缓存：没有指纹表。对调用方来说等同于「每块都要重算」，
		// 但向量本身如果 ID 对得上还是可以复用的——IDs 顺序就是槽位。
		prints = map[string]string{}
	}
	ix := New(m.Dim)
	ix.loadFrom(m.IDs, vecs)
	return ix, prints, nil
}

// loadFrom 用已解出的扁平数组重建块结构（只在 Load 时调用一次）。
func (ix *Index) loadFrom(ids []string, vecs []float32) {
	dim := ix.dim
	var blocks []*vecBlock
	for start := 0; start < len(ids); start += vecBlockCap {
		end := start + vecBlockCap
		if end > len(ids) {
			end = len(ids)
		}
		blk := &vecBlock{n: end - start}
		blk.ids = append(blk.ids, ids[start:end]...)
		lo, hi := start*dim, end*dim
		if hi > len(vecs) {
			hi = len(vecs)
		}
		blk.vecs = append(blk.vecs, vecs[lo:hi]...)
		for len(blk.vecs) < blk.n*dim {
			blk.vecs = append(blk.vecs, 0)
		}
		blocks = append(blocks, blk)
	}
	ix.storeBlocks(blocks)
	for i, id := range ids {
		ix.idMap.Store(id, i)
	}
	ix.count.Store(int64(len(ids)))
}
