//go:build static

package embed

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"unicode"
)

// StaticConfig 指向一个静态词向量表。
type StaticConfig struct {
	// ModelPath 是 .zmstatic 文件。格式见下方 parseStatic。
	ModelPath string
	// Name 是模型标识，写进向量索引的 manifest。
	// 它决定「换模型时缓存是否作废」，所以必须能区分不同模型文件。
	Name string
}

// Static 是一个纯 Go 的「查表 + 平均」嵌入器。
//
// 为什么需要它：进程内 ONNX 需要 CGO 与 libonnxruntime，交叉编译到
// ARM 小盒子会很麻烦；而远端 HTTP 嵌入在断网时不可用。这条路径两者
// 都不要：模型就是一张预先算好的词向量表，运行时只做查表与加权平均，
// 因此 CGO_ENABLED=0 也能跑，一次嵌入在微秒量级。
//
// 代价是没有上下文交互：它看不见「苹果手机」和「苹果很甜」的区别。
// 因此它的定位是**降级兜底**，不是主力检索——检索主力仍然是 BM25
// 命中 + RRF 融合，向量只负责「字面完全不同但意思相通」的那一部分。
type Static struct {
	dim   int
	name  string
	once  sync.Once
	vocab map[string][]float32
	err   error
}

// OpenStatic 读入模型文件。
func OpenStatic(cfg StaticConfig) (*Static, error) {
	if cfg.ModelPath == "" {
		return nil, errors.New("static provider requires model_path pointing at a .zmstatic file")
	}
	s := &Static{name: cfg.Name}
	s.once.Do(func() {
		s.err = s.load(cfg.ModelPath)
		if s.err == nil && s.name == "" {
			s.name = "static:" + cfg.ModelPath
		}
	})
	if s.err != nil {
		return nil, s.err
	}
	return s, nil
}

func (s *Static) load(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read static model %s: %w", path, err)
	}
	dim, vocab, err := parseStatic(raw)
	if err != nil {
		return fmt.Errorf("parse static model %s: %w", path, err)
	}
	s.dim = dim
	s.vocab = vocab
	return nil
}

const staticMagic = "ZMST"

// parseStatic 解析 .zmstatic：
//
//	magic   [4]byte   "ZMST"
//	version uint32le  目前为 1
//	dim     uint32le  向量维度
//	count   uint32le  词表条数
//	条目    count × (len uint16le, token bytes)
//	矩阵    count × dim × float32le
//
// 为什么不用 gob/json：这是要放进小盒子、要能手工导出的资产，
// 布局写死在文档里比「某个 Go 版本的编码细节」可靠。
func parseStatic(raw []byte) (int, map[string][]float32, error) {
	if len(raw) < 16 {
		return 0, nil, errors.New("file too short for a header")
	}
	if string(raw[:4]) != staticMagic {
		return 0, nil, fmt.Errorf("bad magic %q (want %q)", string(raw[:4]), staticMagic)
	}
	ver := binary.LittleEndian.Uint32(raw[4:8])
	if ver != 1 {
		return 0, nil, fmt.Errorf("unsupported version %d", ver)
	}
	dim := int(binary.LittleEndian.Uint32(raw[8:12]))
	count := int(binary.LittleEndian.Uint32(raw[12:16]))
	if dim <= 0 || dim > 8192 {
		return 0, nil, fmt.Errorf("implausible dim %d", dim)
	}
	if count < 0 || count > 5_000_000 {
		return 0, nil, fmt.Errorf("implausible vocab size %d", count)
	}

	off := 16
	tokens := make([]string, count)
	for i := 0; i < count; i++ {
		if off+2 > len(raw) {
			return 0, nil, fmt.Errorf("truncated token length at entry %d", i)
		}
		n := int(binary.LittleEndian.Uint16(raw[off : off+2]))
		off += 2
		if off+n > len(raw) {
			return 0, nil, fmt.Errorf("truncated token at entry %d", i)
		}
		tokens[i] = string(raw[off : off+n])
		off += n
	}

	need := count * dim * 4
	if off+need > len(raw) {
		return 0, nil, fmt.Errorf("truncated matrix: need %d bytes, have %d", need, len(raw)-off)
	}
	vocab := make(map[string][]float32, count)
	for i := 0; i < count; i++ {
		vec := make([]float32, dim)
		for d := 0; d < dim; d++ {
			bits := binary.LittleEndian.Uint32(raw[off : off+4])
			vec[d] = math.Float32frombits(bits)
			off += 4
		}
		vocab[tokens[i]] = vec
	}
	return dim, vocab, nil
}

func (s *Static) Dim() int     { return s.dim }
func (s *Static) Name() string { return s.name }
func (s *Static) Close() error { return nil }

// Embed 把文本变成向量：分词 → 查表 → 加权平均 → L2 归一化。
func (s *Static) Embed(text string) ([]float32, error) {
	out, err := s.EmbedBatch([]string{text})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("static embedder returned no vector")
	}
	return out[0], nil
}

// EmbedBatch 批量嵌入。纯 CPU 查表，没有后端并发限制。
func (s *Static) EmbedBatch(texts []string) ([][]float32, error) {
	if s.vocab == nil {
		return nil, errors.New("static embedder is not loaded")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = s.embedOne(t)
	}
	return out, nil
}

func (s *Static) embedOne(text string) []float32 {
	acc := make([]float32, s.dim)
	var n float32
	for _, tok := range staticTokens(text) {
		vec, ok := s.vocab[tok]
		if !ok {
			continue
		}
		for d, v := range vec {
			acc[d] += v
		}
		n++
	}
	if n == 0 {
		// 一个词都没命中：返回零向量而不是错误。
		// 零向量在余弦检索里得分为 0，等价于「这条不相关」——
		// 这正是我们想要的下游行为，不必让调用方额外处理一种失败。
		return acc
	}
	for d := range acc {
		acc[d] /= n
	}
	return l2normalize(acc)
}

func l2normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// staticTokens 是给静态模型用的分词：小写化 + 切词 + CJK 单字与相邻二元。
//
// 为什么不用现成的 WordPiece：那套词表是给 BERT 用的，与静态模型的
// 词表不对应。这里的策略必须与训练时一致，所以由模型格式约定：
// 静态表按「英文词 + CJK 单字 + CJK 二元」建。
//
// 加 CJK 二元是为了给中文一点词序信息：只有单字时「上海」与「海上」
// 会是同一堆向量；有了二元它们就分得开。
func staticTokens(text string) []string {
	text = strings.ToLower(text)
	var out []string
	var word []rune
	var prevCJK rune

	flush := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			flush()
			out = append(out, string(r))
			if prevCJK != 0 {
				out = append(out, string([]rune{prevCJK, r}))
			}
			prevCJK = r
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			prevCJK = 0
			word = append(word, r)
		default:
			prevCJK = 0
			flush()
		}
	}
	flush()
	return out
}
