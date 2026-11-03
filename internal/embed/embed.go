// Package embed 把一段文字变成向量，纯本机、不联网。
//
// 为什么不用「调用远端 embedding API」：这个库里存的是完整的个人记录，
// 把切片发出去算向量等于把生活流水交给第三方。模型加载进内存后做的是
// 纯数学运算，一个字节都不出这台机器。
//
// 为什么用 ONNX Runtime 而不是纯 Go 推理：实测没有可用的纯 Go 方案。
// 这里只需要 libonnxruntime 提供算子，不需要它参与链接；Go 侧是
// dlopen 的薄封装，CGO 只为了载入动态库。
package embed

import (
	"fmt"
	"math"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// MaxTokens 是一次推理的窗口长度。
//
// 这个是踩过的坑：早先设 64，结果同一轮对话切出来的相邻切片
// 前 64 个 token 完全一样（frontmatter + 引用的同一句提问），
// 得分撞成同一个数字（0.3690 / 0.3690），排序完全失效。
// 512 是 bge / LaBSE 系列的上限，也是让正文真正进入窗口的最小代价。
const MaxTokens = 512

// Embedder 里记录维度：不同模型输出不同（bge 512 / LaBSE 768），
// 首次推理时实测得出，之后所有调用方以 e.Dim() 为准。

// Embedder 是一个加载好的 embedding 会话。
//
// 结构体内含 mutex：ONNX session 不是并发安全的，而索引构建会
// 开多个 goroutine 并行跑。
type Embedder struct {
	mu   sync.Mutex
	sess *ort.DynamicAdvancedSession
	tok  *BertTokenizer
	dim  int // 模型输出维度，首次推理时实测写入
}

// Dim 返回模型输出维度。首次调用会跑一次空推理实测。
func (e *Embedder) Dim() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dim
}

// Config 描述去哪找三样东西：动态库、模型、词表。
type Config struct {
	LibPath   string
	ModelPath string
	VocabPath string
}

// Open 载入运行时与模型。全程本地文件，不发起任何网络请求。
func Open(cfg Config) (*Embedder, error) {
	ort.SetSharedLibraryPath(cfg.LibPath)
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("init onnxruntime (lib=%s): %w", cfg.LibPath, err)
	}
	tok, err := NewBertTokenizer(cfg.VocabPath, MaxTokens)
	if err != nil {
		return nil, fmt.Errorf("load vocab: %w", err)
	}
	sess, err := ort.NewDynamicAdvancedSession(cfg.ModelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"}, nil)
	if err != nil {
		return nil, fmt.Errorf("open model %s: %w", cfg.ModelPath, err)
	}
	// 输出维度不假设：bge-small-zh 是 512，LaBSE 是 768。
	// 从 ONNX 元数据实测，避免「换了模型忘改常量」这种静默错误。
	ins, outs, err := ort.GetInputOutputInfo(cfg.ModelPath)
	if err != nil {
		sess.Destroy()
		return nil, fmt.Errorf("probe model io: %w", err)
	}
	if len(outs) == 0 {
		sess.Destroy()
		return nil, fmt.Errorf("model has no outputs")
	}
	outDims := outs[0].Dimensions
	if len(outDims) != 3 {
		sess.Destroy()
		return nil, fmt.Errorf("unexpected output rank %d (want [batch, seq, dim])", len(outDims))
	}
	dim := int(outDims[2])
	if dim <= 0 {
		sess.Destroy()
		return nil, fmt.Errorf("model output dim is dynamic (%v); cannot determine vector size", outDims)
	}
	_ = ins
	return &Embedder{sess: sess, tok: tok, dim: dim}, nil
}

// Close 释放会话与运行时。
func (e *Embedder) Close() {
	if e.sess != nil {
		e.sess.Destroy()
		e.sess = nil
	}
	ort.DestroyEnvironment()
}

// Embed 把一段文字变成 L2 归一化的向量（模长 1，内积即余弦）。
//
// 池化用 mean pooling：把每个有效 token 的隐状态平均。这是 bge 系列的
// 推荐用法；取 [CLS] 位对这类模型效果更差。
func (e *Embedder) Embed(text string) ([]float32, error) {
	ids, mask, types := e.tok.Encode(text)

	e.mu.Lock()
	defer e.mu.Unlock()

	shape := ort.NewShape(1, int64(MaxTokens))
	inIDs, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, err
	}
	defer inIDs.Destroy()
	inMask, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, err
	}
	defer inMask.Destroy()
	inTypes, err := ort.NewTensor(shape, types)
	if err != nil {
		return nil, err
	}
	defer inTypes.Destroy()

	outShape := ort.NewShape(1, int64(MaxTokens), int64(e.dim))
	out, err := ort.NewEmptyTensor[float32](outShape)
	if err != nil {
		return nil, err
	}
	defer out.Destroy()

	if err := e.sess.Run([]ort.Value{inIDs, inMask, inTypes}, []ort.Value{out}); err != nil {
		return nil, err
	}

	data := out.GetData()
	dim := e.dim
	vec := make([]float32, dim)
	var n float32
	for t := 0; t < MaxTokens; t++ {
		if mask[t] == 0 {
			continue
		}
		base := t * dim
		for d := 0; d < dim; d++ {
			vec[d] += data[base+d]
		}
		n++
	}
	if n == 0 {
		n = 1
	}
	var norm float64
	for d := range vec {
		vec[d] /= n
		norm += float64(vec[d]) * float64(vec[d])
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for d := range vec {
			vec[d] = float32(float64(vec[d]) / norm)
		}
	}
	return vec, nil
}

// Dot 是两个已归一化向量的相似度（内积 = 余弦）。
func Dot(a, b []float32) float32 {
	var s float32
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
