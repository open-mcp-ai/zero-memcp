package embed

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeProvider 是可控的后端：能随时切成失败/恢复。
type fakeProvider struct {
	label string
	dim   int
	down  bool
	calls int
}

func (f *fakeProvider) Dim() int        { return f.dim }
func (f *fakeProvider) Name() string    { return "fake-" + f.label }
func (f *fakeProvider) Close() error    { return nil }
func (f *fakeProvider) Embed(text string) ([]float32, error) {
	f.calls++
	if f.down {
		return nil, errors.New("provider " + f.label + " is down")
	}
	v := make([]float32, f.dim)
	v[0] = 1
	return v, nil
}
func (f *fakeProvider) EmbedBatch(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v, err := f.Embed(texts[i])
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func newTestChain(head, fallback *fakeProvider) *Chain {
	return &Chain{
		providers: []namedEmbedder{
			{label: head.label, emb: head},
			{label: fallback.label, emb: fallback},
		},
		dim:  head.dim,
		name: "test-model",
	}
}

// TestChainClearsStaleError 锁定「恢复后不再报旧错误」。
//
// 曾经的 bug：lastErr 只写不清，于是降级过一次之后，即使链早已
// 完全健康，status 里也会永久挂着那句错误——出现过
// 「ready (7877 vectors) via local-ollama [all providers failed]」
// 这种自相矛盾的输出，用户无法判断该信哪半句。
func TestChainClearsStaleError(t *testing.T) {
	head := &fakeProvider{label: "head", dim: 8, down: true}
	fallback := &fakeProvider{label: "fallback", dim: 8}
	c := newTestChain(head, fallback)

	// 1. 链首挂 → 降级到备用，应有降级记录
	if _, err := c.Embed("hello"); err != nil {
		t.Fatalf("应降级成功: %v", err)
	}
	if !c.Degraded() {
		t.Fatal("链首失败后应处于降级状态")
	}
	if msg, _, ok := c.Note(); !ok || !strings.Contains(msg, "degraded") {
		t.Fatalf("应有降级记录，实际 %q ok=%v", msg, ok)
	}

	// 2. 链首恢复 → HealthCheck 回到链首
	head.down = false
	recovered, err := c.HealthCheck()
	if err != nil || !recovered {
		t.Fatalf("链首恢复后 HealthCheck 应报告恢复: recovered=%v err=%v", recovered, err)
	}
	if c.Degraded() {
		t.Fatal("HealthCheck 成功后不应再是降级状态")
	}
	// 3. 关键断言：降级记录必须已被清除
	if msg, _, ok := c.Note(); ok && msg != "" {
		t.Fatalf("恢复到链首后旧的降级记录必须清除，实际仍为 %q", msg)
	}

	// 4. 链首成功调用也应清除历史
	head.down = true
	_, _ = c.Embed("x") // 再次降级
	head.down = false
	recovered, _ = c.HealthCheck()
	if !recovered {
		t.Fatal("应再次恢复")
	}
	if _, err := c.Embed("y"); err != nil {
		t.Fatalf("健康状态下的调用应成功: %v", err)
	}
	if msg, _, ok := c.Note(); ok && msg != "" {
		t.Fatalf("链首成功后不应残留错误，实际 %q", msg)
	}
}

// TestChainHealthCheckOnlyProbesHead 锁定「已在链首时不重复探测」。
func TestChainHealthCheckOnlyProbesHead(t *testing.T) {
	head := &fakeProvider{label: "head", dim: 8}
	fallback := &fakeProvider{label: "fallback", dim: 8}
	c := newTestChain(head, fallback)

	recovered, err := c.HealthCheck()
	if err != nil || recovered {
		t.Fatalf("已在链首时 HealthCheck 应无事可做: recovered=%v err=%v", recovered, err)
	}
	if head.calls != 0 {
		t.Fatalf("已在链首时不应发起探测，实际调用 %d 次", head.calls)
	}
}

// TestChainNoteHasTimestamp 锁定「记录带时间」——没有时间就无法
// 区分「现在降级」和「很久以前降级过」。
func TestChainNoteHasTimestamp(t *testing.T) {
	head := &fakeProvider{label: "head", dim: 8, down: true}
	fallback := &fakeProvider{label: "fallback", dim: 8}
	c := newTestChain(head, fallback)

	if _, err := c.Embed("x"); err != nil {
		t.Fatal(err)
	}
	_, at, ok := c.Note()
	if !ok {
		t.Fatal("应有记录")
	}
	if time.Since(at) > time.Minute {
		t.Fatalf("记录时间应是刚刚，实际 %s 前", time.Since(at))
	}
}

// TestChainRecoversProviderMissingAtStartup 锁定最真实的场景：
//
//	3060 关机 → 启动 zero-memcp（链首连不上，被丢弃）
//	→ 3060 开机 → 后台巡检应把它重新接回链首
//
// 这是「降级链」的核心价值。早先 OpenChain 把启动时连不上的后端
// 永久丢弃，于是这种情况下的恢复永远不会发生——HealthCheck 根本
// 没有可探测的对象，Degraded() 也永远是 false。
//
// 单元测试最初没抓到它，因为手工构造 Chain 绕过了 OpenChain 的裁剪。
func TestChainRecoversProviderMissingAtStartup(t *testing.T) {
	// 用一个可控制的"端口"：先不监听，OpenChain 时链首必然失败。
	head := &fakeProvider{label: "head", dim: 8, down: true}
	fallback := &fakeProvider{label: "fallback", dim: 8}

	// 模拟 OpenChain 的裁剪结果：只留下能连上的那个。
	c := &Chain{
		cfg: ChainConfig{Providers: []ProviderConfig{
			{Label: "head", Kind: "fake"},
			{Label: "fallback", Kind: "fake"},
		}},
		providers: []namedEmbedder{{label: "fallback", emb: fallback}},
		dim:       8,
		name:      "test-model",
	}
	_ = head

	// 此时链上只有 fallback，active=0（它自己就是链首）。
	if got := c.ActiveLabel(); got != "fallback" {
		t.Fatalf("启动时链首应退化为 fallback，实际 %q", got)
	}
	if len(c.Providers()) != 1 {
		t.Fatalf("链上应只有 1 个后端，实际 %v", c.Providers())
	}

	// 关键：此时 Degraded() 为 false（因为它就在自己位置的 0 上），
	// 但链是残缺的。HealthCheck 必须能发现并尝试补回。
	//
	// 用一个真实可连的 HTTP 后端来验证"重新接回"这一步：
	// openProvider 对 fake kind 不支持，所以这里验证「尝试」而不是「成功」。
	// 失败也必须返回错误，让上层知道链首仍不可用——不能静默成功。
	recovered, err := c.HealthCheck()
	if recovered {
		t.Fatal("链首仍未恢复，不应报告 recovered")
	}
	if err == nil {
		t.Fatal("链首不可用时必须返回错误，不能静默成功")
	}
}

// TestChainDegradedReportsTrueWhenNotAtHead 锁定 Degraded 的语义：
// 「不在链首」= 正在用备用后端，status 该报降级原因。
func TestChainDegradedReportsTrueWhenNotAtHead(t *testing.T) {
	head := &fakeProvider{label: "head", dim: 8, down: true}
	fallback := &fakeProvider{label: "fallback", dim: 8}
	c := newTestChain(head, fallback)

	if c.Degraded() {
		t.Fatal("初始应在链首")
	}
	if _, err := c.Embed("x"); err != nil {
		t.Fatal(err)
	}
	if !c.Degraded() {
		t.Fatal("降级到备用后 Degraded 应为 true")
	}
	if got := c.ActiveLabel(); got != "fallback" {
		t.Fatalf("当前后端应为 fallback，实际 %q", got)
	}
}
