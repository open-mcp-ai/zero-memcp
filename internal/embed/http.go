package embed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPEmbedder 通过 OpenAI 兼容的 /v1/embeddings 接口做向量化。
//
// 为什么选 OpenAI 协议而不是各家的原生协议：
// TEI、Ollama、vLLM、LM Studio、llama.cpp server 都实现了
// POST /v1/embeddings {"input": [...], "model": "..."}，
// 换后端只改 endpoint，代码不动。这是「不绑定任何一家」的最低成本做法。
//
// 主进程因此可以完全不带 CGO：交叉编译、容器化、Windows 原生
// 都不再需要对应平台的 onnxruntime 动态库。
type HTTPEmbedder struct {
	endpoint string // 已规范化，不含尾斜杠
	model    string
	dim      int
	apiKey   string
	client   *http.Client
}

// HTTPConfig 配置 HTTP 后端。
type HTTPConfig struct {
	// Endpoint 是基地址，如 http://127.0.0.1:8080。
	// 若已含 /v1/embeddings 则直接使用。
	Endpoint string
	// Model 是模型标识。TEI 忽略它（模型在启动时固定），
	// Ollama 用它选模型。
	Model string
	// Dim 是期望的向量维度。为 0 时由首次响应推断。
	Dim int
	// APIKey 是 Bearer token（可选）。
	APIKey string
	// Timeout 是单次请求超时。远端应设短，让断网快速降级。
	Timeout time.Duration
}

// OpenHTTP 建立 HTTP 后端并探测可用性。
//
// 探测是必须的：配置写着「3060 那台」，但那台机器可能关着。
// 不探测就把它放进链首，每次检索都要先吃一次超时。
func OpenHTTP(cfg HTTPConfig) (*HTTPEmbedder, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("http embedder requires an endpoint")
	}
	ep := strings.TrimRight(cfg.Endpoint, "/")
	if !strings.HasSuffix(ep, "/v1/embeddings") && !strings.HasSuffix(ep, "/embeddings") {
		ep += "/v1/embeddings"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	e := &HTTPEmbedder{
		endpoint: ep,
		model:    cfg.Model,
		dim:      cfg.Dim,
		apiKey:   cfg.APIKey,
		client:   &http.Client{Timeout: timeout},
	}
	// 探测：一次真实的小请求，确认服务在、模型对、维度对得上。
	probe, err := e.Embed("ping")
	if err != nil {
		return nil, fmt.Errorf("probe %s: %w", ep, err)
	}
	if e.dim == 0 {
		e.dim = len(probe)
	} else if len(probe) != e.dim {
		return nil, fmt.Errorf(
			"%s returned dim %d but %d was configured (model mismatch?)",
			ep, len(probe), e.dim)
	}
	return e, nil
}

// Dim 返回向量维度。
func (e *HTTPEmbedder) Dim() int { return e.dim }

// Name 返回模型标识，写进向量缓存 manifest。
func (e *HTTPEmbedder) Name() string {
	if e.model != "" {
		return e.model
	}
	// 没配模型名时用 endpoint 兜底：至少让「换了后端」能被识别为缓存失效。
	return "http:" + e.endpoint
}

// Close 释放连接池。
func (e *HTTPEmbedder) Close() error {
	e.client.CloseIdleConnections()
	return nil
}

// Embed 单条向量化。
func (e *HTTPEmbedder) Embed(text string) ([]float32, error) {
	vs, err := e.EmbedBatch([]string{text})
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	return vs[0], nil
}

// EmbedBatch 批量向量化。
//
// 批量是这个后端存在的最大理由：建索引时 5,000+ 片，
// 逐条 HTTP 往返的开销（哪怕本地回环）会盖过推理本身。
// TEI 的动态批处理要一次拿到多条才能真正并发起来。
func (e *HTTPEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body := map[string]any{
		"input": texts,
		// 显式要求 float，别让服务端返回 base64 或 int8
		"encoding_format": "float",
	}
	if e.model != "" {
		body["model"] = e.model
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", e.endpoint, resp.StatusCode, truncate(string(raw), 300))
	}

	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse embeddings response: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("asked for %d embeddings, got %d", len(texts), len(out.Data))
	}

	// 按 index 归位：服务端可能乱序返回（并发批处理时常见）
	vs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vs) {
			return nil, fmt.Errorf("response index %d out of range (want < %d)", d.Index, len(vs))
		}
		if len(d.Embedding) != e.dim {
			return nil, fmt.Errorf("embedding %d has dim %d, want %d", d.Index, len(d.Embedding), e.dim)
		}
		vs[d.Index] = d.Embedding
	}
	for i, v := range vs {
		if v == nil {
			return nil, fmt.Errorf("response missing index %d", i)
		}
	}
	return vs, nil
}

// Health 探测后端是否可用（供恢复检查用）。
func (e *HTTPEmbedder) Health() error {
	_, err := e.Embed("ping")
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
