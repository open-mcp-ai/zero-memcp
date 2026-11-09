package server

import (
	"sync"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/embed"
	"github.com/open-mcp-ai/zero-memcp/internal/vecindex"
)

// 语义索引的后台维护：把「启动时构建一次」改成「持续追平」。
//
// 旧行为有两个硬伤，都来自「只在启动时构建一次」这个假设：
//
//  1. 启动时嵌入服务不可用（Ollama 没起、3060 断网），语义索引就
//     永远是空的，直到下次重启。期间写入的记忆没有向量，检索只能走
//     BM25——没有任何机制会补，也没有任何东西会报「欠着没补」。
//
//  2. 启动之后写入的记忆要等下次重启才进索引。日常写入是常态，
//     这意味着「刚记下的东西搜不到」。
//
// 新模型：一个后台协程，周期性地把索引追平到当前树状态。追平本身
// 就是增量（按内容指纹比对，见 buildSemanticIndex），所以「无事可做」
// 的周期只是一次全树遍历加指纹比对，几千片的代价是几十毫秒。
//
// 为什么不在写入时同步算向量：写入是热路径。一次 HTTP 往返打进去
// 会让 put 从毫秒变几百毫秒，而嵌入服务抖动会直接把写入也拖失败。
// 索引是派生数据，晚几秒追平没有正确性问题；写入失败有。
//
// 自愈的来源很朴素：构建失败的块不进 prints，于是下个周期的比对
// 会把它们认成「新增」，再试一次。恢复即补算，不需要专门的补偿逻辑。

// semanticMaintainer 管理后台追平协程。零值可用（未启动状态）。
type semanticMaintainer struct {
	mu   sync.Mutex
	stop chan struct{}
	kick chan struct{} // 容量 1：写入后请求尽快追平
}

func (m *semanticMaintainer) stopLocked() {
	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
	m.kick = nil
}

// startSemanticMaintainer 启动后台追平。interval<=0 用默认 60s。
func (s *Server) startSemanticMaintainer(interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	m := &s.semanticMaint
	m.mu.Lock()
	if m.stop != nil {
		m.mu.Unlock()
		return // 已经在跑
	}
	stop := make(chan struct{})
	kick := make(chan struct{}, 1)
	m.stop, m.kick = stop, kick
	m.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.catchUpSemantic("periodic")
			case <-kick:
				// 写入触发：稍微攒一下，连续写入不逐条追平。
				select {
				case <-time.After(3 * time.Second):
				case <-stop:
					return
				}
				s.catchUpSemantic("write")
			}
		}
	}()
}

// stopSemanticMaintainer 停掉后台协程（Close 时调用）。
func (s *Server) stopSemanticMaintainer() {
	m := &s.semanticMaint
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

// kickSemantic 请求尽快追平（写入后调用）。非阻塞：
// 通道满了说明已有一笔待执行，全量比对一次就够，丢掉这笔请求。
func (s *Server) kickSemantic() {
	m := &s.semanticMaint
	m.mu.Lock()
	kick := m.kick
	m.mu.Unlock()
	if kick == nil {
		return
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

// catchUpSemantic 把语义索引追平到当前树状态。
//
// 这是「嵌入失败后自动补」的落点。它不假设嵌入服务可用，
// 而是每次真的去试：失败就记下错误等下个周期；恢复后，
// 之前没算成的那批会因为「指纹在 prints 里查不到」被认成新增，
// 走同一条增量路径补上。
func (s *Server) catchUpSemantic(reason string) {
	// 后端为 nil（启动时全链不可用）→ 每个周期尝试重建链。
	s.semantic.mu.RLock()
	emb := s.semantic.emb
	s.semantic.mu.RUnlock()
	if emb == nil {
		chain, err := embed.OpenChain(s.semanticChainConfig())
		if err == nil {
			chain.OnTransition(func(m string) { s.logf("%s", m) })
		}
		if err != nil {
			// 仍然不可用：记下原因，等下个周期。这不是错误噪音，
			// 是「欠着没补」的唯一可见痕迹。
			s.semantic.mu.Lock()
			s.semantic.err = "embedder unavailable: " + err.Error()
			s.semantic.mu.Unlock()
			return
		}
		s.semantic.mu.Lock()
		s.semantic.emb = chain
		s.semantic.err = ""
		s.semantic.mu.Unlock()
		s.logf("semantic: embedder recovered (dim=%d, model=%s, active=%s)",
			chain.Dim(), chain.Name(), chain.ActiveLabel())
		emb = chain
	}

	// 粘性降级恢复：如果当前在用备用后端，探一下链首是否回来了。
	//
	// 这是「3060 断网降级到本地，3060 回来后该回 GPU」的唯一实现点。
	// 早先 Chain 的注释写着「见 health.go」，而那个文件从未存在——
	// 于是链一旦降级就永久不回头，把临时故障变成了永久迁移。
	// 无条件调用，不在外面加条件。
	//
	// 踩过的坑：这里早先写成 `if ch.Degraded()`。而 Degraded() 是
	// 「active 不在下标 0」，链首**被 OpenChain 丢弃**时 active 恰好是 0，
	// 于是条件永远为假，恢复逻辑一次都没跑过——现象是「3060 回来了但
	// 系统还在用本地」，而且日志里连一行提示都没有。
	//
	// 该不该探测的判断属于 Chain 自己（它知道配置里应该有几个后端），
	// 调用方只负责按周期去敲这个门。
	if ch, ok := emb.(*embed.Chain); ok {
		// 仍在降级中时不刷日志：semanticStatus / status 里能看到原因，
		// 每周期打一行只会把真正重要的迁移日志淹掉。
		if recovered, _ := ch.HealthCheck(); recovered {
			s.logf("semantic: recovered to chain head %q (providers=%v)",
				ch.ActiveLabel(), ch.Providers())
		}
	}

	// 正在构建时跳过本轮（避免两个构建并发跑）。
	s.semantic.mu.Lock()
	if s.semantic.building {
		s.semantic.mu.Unlock()
		return
	}
	s.semantic.mu.Unlock()

	vecDir := s.semanticDir
	heads := s.semanticHeads()

	// 增量比对的基准：当前缓存 + 它的指纹表。
	var cached *vecindex.Index
	var prints map[string]string
	if vecDir != "" {
		if ix, pr, err := vecindex.Load(vecDir, emb.Name()); err == nil && ix.Dim() == emb.Dim() {
			cached, prints = ix, pr
		}
	}

	// 内存里挂着的索引可能比磁盘新（上一轮追平刚写完还没落盘），
	// 用内存版做基准，别让刚算完的向量被重算。
	s.semantic.mu.RLock()
	if s.semantic.idx != nil {
		cached = s.semantic.idx
	}
	s.semantic.mu.RUnlock()

	if delta := s.semanticDelta(cached, prints); delta.needsEmbed == 0 && delta.removed == 0 && cached != nil {
		// 无事可做。上次有错误的话清掉。
		s.semantic.mu.Lock()
		s.semantic.err = ""
		s.semantic.mu.Unlock()
		return
	}

	s.semantic.mu.Lock()
	s.semantic.building = true
	s.semantic.mu.Unlock()
	go func() {
		defer func() {
			s.semantic.mu.Lock()
			s.semantic.building = false
			s.semantic.mu.Unlock()
		}()
		if err := s.buildSemanticIndex(vecDir, heads, cached, prints); err != nil {
			s.semantic.mu.Lock()
			s.semantic.err = err.Error()
			s.semantic.mu.Unlock()
			return
		}
		s.semantic.mu.Lock()
		s.semantic.err = ""
		s.semantic.mu.Unlock()
	}()
}

// semanticDeltaStats 是一次追平前算出的差量规模。
type semanticDeltaStats struct {
	needsEmbed int // 指纹变了或新增，需要推理
	reusable   int // 指纹没变，可直接复用
	removed    int // 索引里有、树里已经没有
}

// semanticDelta 只做比对，不推理。
func (s *Server) semanticDelta(cached *vecindex.Index, prints map[string]string) semanticDeltaStats {
	var st semanticDeltaStats
	seen := make(map[string]bool, len(prints)+64)
	for _, t := range s.snapshotTrees() {
		for _, b := range t.Indexable() {
			if !isSemanticContent(b.Path) {
				continue
			}
			data, err := s.readBlock(b)
			if err != nil {
				continue
			}
			text := semanticText(string(data))
			if text == "" {
				continue
			}
			id := b.ID()
			seen[id] = true
			if old, ok := prints[id]; ok && old == fingerprint(text) && cached != nil && cached.Has(id) {
				st.reusable++
			} else {
				st.needsEmbed++
			}
		}
	}
	for id := range prints {
		if !seen[id] {
			st.removed++
		}
	}
	return st
}
