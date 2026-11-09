// Package embed 提供文本向量化。两条路径：
//
//	LocalONNX  —— 进程内 ONNX Runtime（CGO dlopen），零外部进程，
//	              但二进制绑平台、模型 tokenizer 受限。
//	RemoteHTTP —— OpenAI 兼容的 /v1/embeddings（TEI / Ollama / 自建），
//	              主进程变纯 Go，可交叉编译，支持批量与 GPU。
//
// 两者实现同一个 Embedder 接口，调用方不关心后端在哪。
package embed

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Embedder 是向量化后端的统一接口。
type Embedder interface {
	// Dim 返回向量维度。降级链上所有后端必须一致，否则向量不可比。
	Dim() int
	// Embed 把一段文本变成向量。
	Embed(text string) ([]float32, error)
	// EmbedBatch 批量向量化。单条实现可以退化成循环，
	// 但 HTTP 后端应真正利用批量——建索引时这是数量级差异。
	EmbedBatch(texts []string) ([][]float32, error)
	// Name 是写进向量缓存 manifest 的模型标识。
	// 换模型必须换名字，旧缓存才会自动判过期。
	Name() string
	// Close 释放资源。
	Close() error
}

// ErrAllProvidersDown 表示降级链上所有后端都不可用。
var ErrAllProvidersDown = errors.New("all embedding providers are unavailable")

// Chain 是按优先级排列的后端链，前面的不可用就降到后面的。
//
// 为什么需要它：目标环境是「3060 主机跑 GPU 推理 + Mac 笔记本本地兜底」。
// 3060 在局域网里，会断网、会休眠、会重启；每次都让人手工改配置不现实。
//
// 一条硬约束：链上所有后端必须**同模型同维度**。不同模型算出的向量
// 不可比，混在一个索引里检索会直接失效。Open 会校验这一点。
type Chain struct {
	// mu 保护 providers / dim / name。
	//
	// 为什么需要：降级链不再是「打开后即不变」的静态结构。启动时连不上
	// 的后端不能永久丢弃——3060 关机时启动服务，之后再开机，它就该重新
	// 被用上。这要求运行期能重建 providers，而 Embed 在热路径上读它。
	mu        sync.RWMutex
	cfg       ChainConfig
	providers []namedEmbedder
	dim       int
	name      string
	// onTransition 在 active 后端发生切换时被调用（降级 / 全部失败 / 恢复）。
	//
	// 为什么需要它：降级记录写在 note 里、能从 status 看到，但**降级发生
	// 的那一刻日志里什么都没有**。排查时看到的是「服务一直在用备用后端」，
	// 却不知道它是几点切的、切的原因是什么。Chain 在 embed 包，不认识
	// 服务的 logger，所以由调用方注入一个回调。
	//
	// 只在切换时调用，不是每次失败都调——真正的迁移才是值得记的事件，
	// 每周期刷一行会把恢复日志淹掉。
	onTransition func(msg string)

	// active 是当前生效的后端下标（atomic，热路径读）。
	active atomic.Int32
	// note 记录最近一次降级/失败的原因和时间，供 status 报告。
	//
	// 必须带时间：只存字符串的话，一次瞬时失败会永远挂在 status 上，
	// 出现「ready ... [all providers failed]」这种自相矛盾的输出，
	// 让人无法判断该信哪半句。
	note atomic.Value // chainNote
}

// chainNote 是一次降级或失败的记录。
type chainNote struct {
	msg string
	at  time.Time
}

type namedEmbedder struct {
	label string
	emb   Embedder
}

// ChainConfig 描述一条降级链。
type ChainConfig struct {
	// Providers 按优先级排列。空表示不启用语义检索。
	Providers []ProviderConfig
}

// ProviderConfig 是链上的一个后端。
type ProviderConfig struct {
	// Label 是诊断用的名字（"3060"、"local"），出现在日志和 status 里。
	Label string
	// Kind 是 "http" 或 "onnx"。
	Kind string
	// Model 是这个后端的模型标识。必须与链上其他后端一致。
	Model string
	// Dim 是向量维度。必须与链上其他后端一致。
	Dim int
	// Endpoint 是 HTTP 后端的基地址（如 http://127.0.0.1:8080）。
	Endpoint string
	// APIKey 是 HTTP 后端的 Bearer token（可选）。
	APIKey string
	// Timeout 是单次请求超时。远端机器应设短一些，
	// 让「断网」快速降级而不是让检索卡住。
	Timeout time.Duration
	// ONNX 后端专用路径
	LibPath   string
	ModelPath string
	VocabPath string
}

// OpenChain 按配置建立降级链。
//
// 只要求至少一个后端可用：链首不可用会继续尝试后续的。
// 全部不可用返回 ErrAllProvidersDown，调用方据此关闭语义检索
// （而不是让服务起不来——BM25 检索始终可用）。
func OpenChain(cfg ChainConfig) (*Chain, error) {
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("no embedding providers configured")
	}

	c := &Chain{cfg: cfg}
	var firstErr error
	for _, pc := range cfg.Providers {
		label := pc.Label
		if label == "" {
			label = pc.Kind
		}
		emb, err := openProvider(pc)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", label, err)
			}
			continue
		}
		// 维度与模型一致性校验：不同模型的向量不可比。
		//
		// 这里的选择是硬报错而不是跳过。区别在于：
		//   - 连不上 = 可用性问题，跳过、降级，这是链存在的理由；
		//   - 维度不一致 = 配置写错了，必须大声失败。
		// 静默跳过一个「配了但维度不对」的后端，结果就是你以为在用 3060，
		// 实际一直在用本地——而且检索结果看着正常，只是准不准不知道。
		if c.dim == 0 {
			c.dim = emb.Dim()
			c.name = emb.Name()
		} else if emb.Dim() != c.dim {
			_ = emb.Close()
			return nil, fmt.Errorf(
				"provider %q: dim %d != %d (a chain requires ONE model across all providers; "+
					"vectors from different models are not comparable)",
				label, emb.Dim(), c.dim)
		} else if emb.Name() != c.name {
			_ = emb.Close()
			return nil, fmt.Errorf(
				"provider %q: model %q != %q (a chain requires ONE model across all providers)",
				label, emb.Name(), c.name)
		}
		c.providers = append(c.providers, namedEmbedder{label: label, emb: emb})
	}
	if len(c.providers) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrAllProvidersDown, firstErr)
		}
		return nil, ErrAllProvidersDown
	}
	return c, nil
}

// labelOf 取配置项的显示名（与 OpenChain 里的规则一致）。
func labelOf(pc ProviderConfig) string {
	if pc.Label != "" {
		return pc.Label
	}
	return pc.Kind
}

func openProvider(pc ProviderConfig) (Embedder, error) {
	switch strings.ToLower(pc.Kind) {
	case "http":
		return OpenHTTP(HTTPConfig{
			Endpoint: pc.Endpoint,
			Model:    pc.Model,
			Dim:      pc.Dim,
			APIKey:   pc.APIKey,
			Timeout:  pc.Timeout,
		})
	case "onnx", "":
		e, err := OpenLocal(Config{
			LibPath:   pc.LibPath,
			ModelPath: pc.ModelPath,
			VocabPath: pc.VocabPath,
		})
		if err != nil {
			return nil, err
		}
		return e, nil
	default:
		return nil, fmt.Errorf("unknown provider kind %q (want http or onnx)", pc.Kind)
	}
}

// Dim 返回链的向量维度。
func (c *Chain) Dim() int { return c.dim }

// Name 返回链的模型标识。
func (c *Chain) Name() string { return c.name }

// ActiveLabel 返回当前生效的后端名，供 status 展示。
func (c *Chain) ActiveLabel() string {
	ps := c.snapshot()
	i := int(c.active.Load())
	if i < 0 || i >= len(ps) {
		return ""
	}
	return ps[i].label
}

// LastError 返回最近一次降级的原因；从未降级时返回空串。
func (c *Chain) LastError() string {
	if v, ok := c.note.Load().(chainNote); ok {
		return v.msg
	}
	return ""
}

// Note 返回最近一次降级/失败的原因及其发生时间。
func (c *Chain) Note() (string, time.Time, bool) {
	if v, ok := c.note.Load().(chainNote); ok && v.msg != "" {
		return v.msg, v.at, true
	}
	return "", time.Time{}, false
}

// Degraded 报告当前是否不在链首（即正在使用备用后端）。
func (c *Chain) Degraded() bool { return c.active.Load() != 0 }

// transition 把一次后端切换交给调用方的日志回调。
//
// 回调是可选的：Chain 单独使用（测试、嵌入方只要能力不要日志）时
// 保持安静，不给它强加一个 logger。
func (c *Chain) transition(msg string) {
	if c.onTransition != nil {
		c.onTransition(msg)
	}
}

// OnTransition 注入切换回调。必须在 Chain 开始被并发使用之前调用。
func (c *Chain) OnTransition(fn func(string)) { c.onTransition = fn }

// markRecovered 在链首成功时清掉历史错误。
//
// 不清的后果不是"多显示一行日志"，而是让人永远无法确认系统到底恢没恢复：
// 一次瞬时超时会永久挂在 status 上，和 ready 并列显示。
func (c *Chain) markRecovered(atHead bool) {
	if atHead {
		c.note.Store(chainNote{})
	}
}

// snapshot 取当前生效后端列表的副本。
func (c *Chain) snapshot() []namedEmbedder {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.providers
}

// Providers 返回链上所有后端的标签，按优先级排列。
func (c *Chain) Providers() []string {
	ps := c.snapshot()
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.label
	}
	return out
}

// Embed 按优先级尝试，失败则降级到下一个可用后端。
//
// 降级是**粘性**的：一旦降级成功，active 移到新位置，
// 后续请求直接从那里开始，不必每次都先等链首超时。
// 恢复由 HealthCheck 负责（链首重新可用时把 active 移回去）。
func (c *Chain) Embed(text string) ([]float32, error) {
	ps := c.snapshot()
	start := int(c.active.Load())
	if start >= len(ps) {
		start = 0
	}
	var lastErr error
	for i := start; i < len(ps); i++ {
		v, err := ps[i].emb.Embed(text)
		if err == nil {
			if i != start {
				c.active.Store(int32(i))
				msg := fmt.Sprintf("degraded to %s: %v", ps[i].label, lastErr)
				c.note.Store(chainNote{msg: msg, at: time.Now()})
				c.transition("semantic: " + msg)
			}
			// 链首成功 = 完全健康，清掉历史错误。
			c.markRecovered(i == 0)
			return v, nil
		}
		lastErr = err
	}
	// 链尾也失败了：重置到链首，让下次请求重试最优后端
	msg := fmt.Sprintf("all providers failed: %v", lastErr)
	c.active.Store(0)
	c.note.Store(chainNote{msg: msg, at: time.Now()})
	c.transition("semantic: " + msg)
	return nil, fmt.Errorf("%w: %v", ErrAllProvidersDown, lastErr)
}

// EmbedBatch 批量向量化，带降级。
func (c *Chain) EmbedBatch(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	ps := c.snapshot()
	start := int(c.active.Load())
	if start >= len(ps) {
		start = 0
	}
	var lastErr error
	for i := start; i < len(ps); i++ {
		vs, err := ps[i].emb.EmbedBatch(texts)
		if err == nil {
			if i != start {
				c.active.Store(int32(i))
				msg := fmt.Sprintf("degraded to %s: %v", ps[i].label, lastErr)
				c.note.Store(chainNote{msg: msg, at: time.Now()})
				c.transition("semantic: " + msg)
			}
			c.markRecovered(i == 0)
			return vs, nil
		}
		lastErr = err
	}
	c.active.Store(0)
	c.note.Store(chainNote{msg: fmt.Sprintf("all providers failed: %v", lastErr), at: time.Now()})
	return nil, fmt.Errorf("%w: %v", ErrAllProvidersDown, lastErr)
}

// Close 关闭链上所有后端。
func (c *Chain) Close() error {
	var firstErr error
	for _, p := range c.providers {
		if err := p.emb.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// HealthCheck 探测链首是否恢复，恢复则把 active 移回去。
//
// # 为什么必须有它
//
// 降级是粘性的：3060 断网一次，active 就永久停在本地 Ollama 上。
// 没有恢复机制的话，「3060 回来了继续用 GPU」这件事永远不会发生——
// 而链首通常就是快的那一个，粘着不走等于把降级当成了永久迁移。
//
// 早先这个函数的注释里写着「见 health.go」，而 health.go 从未存在，
// 于是链一旦降级就再也不回头。
//
// # 探测方式
//
// 只探链首：链尾降级本来就该一直生效，没必要探。探测用一条极短文本，
// 代价是一次本地/局域网往返。失败不影响当前后端——继续用备用就好。
func (c *Chain) HealthCheck() (recovered bool, err error) {
	ps := c.snapshot()
	// 已经在链首且链上没有任何缺失的后端 → 无事可做。
	if len(ps) == 0 {
		return false, nil
	}
	// 没有配置记录就无法知道"缺了谁"（手工构造的 Chain、或旧数据）。
	// 这种情况下只做「链首还活着吗」的探测，不去猜配置。
	if len(c.cfg.Providers) == 0 {
		if c.active.Load() == 0 {
			return false, nil
		}
		if _, herr := ps[0].emb.Embed("ping"); herr != nil {
			return false, herr
		}
		c.active.Store(0)
		c.note.Store(chainNote{})
		return true, nil
	}
	if c.active.Load() == 0 && len(ps) == len(c.cfg.Providers) {
		return false, nil
	}

	// 情况一：链首还在链上（只是 active 被降级了）——直接探测。
	if c.active.Load() != 0 && len(ps) > 0 {
		if _, herr := ps[0].emb.Embed("ping"); herr == nil {
			c.active.Store(0)
			c.note.Store(chainNote{})
			return true, nil
		} else {
			return false, herr
		}
	}

	// 情况二：链首在启动时就连不上，已被 OpenChain 丢弃。
	//
	// 这是最常见的真实场景：3060 关机时启动服务，之后再开机。
	// 早先 OpenChain 把连不上的后端永久丢弃，于是「恢复」永远不会发生——
	// HealthCheck 根本没有可探测的对象，Degraded() 也永远是 false。
	// 这里重新尝试打开配置里的链首，成功就插回链的最前面。
	want := c.cfg.Providers[0]
	label := labelOf(want)
	// 已经在链上了（前面的分支处理过），说明是别的情况
	for _, p := range ps {
		if p.label == label {
			return false, nil
		}
	}
	emb, oerr := openProvider(want)
	if oerr != nil {
		return false, oerr
	}
	// 维度/模型必须与现有链一致，否则插进去会污染索引。
	if emb.Dim() != c.dim || emb.Name() != c.name {
		_ = emb.Close()
		return false, fmt.Errorf(
			"recovered provider %q does not match chain (%d/%s vs %d/%s)",
			label, emb.Dim(), emb.Name(), c.dim, c.name)
	}

	c.mu.Lock()
	c.providers = append([]namedEmbedder{{label: label, emb: emb}}, c.providers...)
	c.mu.Unlock()
	c.active.Store(0)
	c.note.Store(chainNote{})
	return true, nil
}
