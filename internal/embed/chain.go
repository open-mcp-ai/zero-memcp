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
	providers []namedEmbedder
	dim       int
	name      string

	// active 是当前生效的后端下标（atomic，热路径读）。
	active atomic.Int32
	// lastErr 记录最近一次降级的原因，供 status 报告。
	lastErr atomic.Value // string
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

	c := &Chain{}
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
	i := int(c.active.Load())
	if i < 0 || i >= len(c.providers) {
		return ""
	}
	return c.providers[i].label
}

// LastError 返回最近一次降级的原因；从未降级时返回空串。
func (c *Chain) LastError() string {
	if v, ok := c.lastErr.Load().(string); ok {
		return v
	}
	return ""
}

// Providers 返回链上所有后端的标签，按优先级排列。
func (c *Chain) Providers() []string {
	out := make([]string, len(c.providers))
	for i, p := range c.providers {
		out[i] = p.label
	}
	return out
}

// Embed 按优先级尝试，失败则降级到下一个可用后端。
//
// 降级是**粘性**的：一旦降级成功，active 移到新位置，
// 后续请求直接从那里开始，不必每次都先等链首超时。
// 恢复由 HealthCheck 负责（见 health.go）。
func (c *Chain) Embed(text string) ([]float32, error) {
	start := int(c.active.Load())
	var lastErr error
	for i := start; i < len(c.providers); i++ {
		v, err := c.providers[i].emb.Embed(text)
		if err == nil {
			if i != start {
				c.active.Store(int32(i))
				c.lastErr.Store(fmt.Sprintf("degraded to %s: %v", c.providers[i].label, lastErr))
			}
			return v, nil
		}
		lastErr = err
	}
	// 链尾也失败了：重置到链首，让下次请求重试最优后端
	c.active.Store(0)
	c.lastErr.Store(fmt.Sprintf("all providers failed: %v", lastErr))
	return nil, fmt.Errorf("%w: %v", ErrAllProvidersDown, lastErr)
}

// EmbedBatch 批量向量化，带降级。
func (c *Chain) EmbedBatch(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	start := int(c.active.Load())
	var lastErr error
	for i := start; i < len(c.providers); i++ {
		vs, err := c.providers[i].emb.EmbedBatch(texts)
		if err == nil {
			if i != start {
				c.active.Store(int32(i))
				c.lastErr.Store(fmt.Sprintf("degraded to %s: %v", c.providers[i].label, lastErr))
			}
			return vs, nil
		}
		lastErr = err
	}
	c.active.Store(0)
	c.lastErr.Store(fmt.Sprintf("all providers failed: %v", lastErr))
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
