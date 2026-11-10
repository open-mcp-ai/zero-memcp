// semantic.go —— 向量语义检索：让「中文的问法」摸到「英文的思考」。
//
// 为什么需要它：2026-10-28 07:43:10 那篇 thinking 里写着选端口
// 1729 的理由（Hardy–Ramanujan 数，一个数字两条路径可达，正好映射
// 联想记忆）。它一直在库里，但 BM25 永远搜不到——提问是中文，
// 那段推理是英文，字面零重叠。
//
// 为什么只索引 thought 分支：语义的价值在「为什么」，而「为什么」
// 只写在推理链里。record 里的结论 BM25 已经够用，且中文对中文
// 命中会把跨语言的 thinking 压下去（实测：中文问「端口为什么是
// 1729」，中文 record 得 0.52，英文 thought 只得 0.43）。
// 所以这两层不是二选一，而是必须成对召回——见 srecall 的回链逻辑。

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/embed"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
	"github.com/open-mcp-ai/zero-memcp/internal/vecindex"
)

// isSemanticContent 判断一条内容是否参与语义索引。
//
// 按**路径**判断，不按分支名。
//
// 这是地址语法改成 `ref:path` 之后的关键变化：分支只表达「哪条生命线」，
// 不再需要靠分支名去猜里面装什么内容。早先写成 isSemanticBranch，
// 靠分支前缀（thought-* / record-*）识别，导致每加一个人就要改代码、
// 重建索引、重新发版——而且分支名一旦不再携带内容语义，那个函数就全错。
//
// 参与索引的路径：
//   thought/  推理链（回答「为什么」）
//   record/   一问一答（回答「发生过什么」）
//   review/   人工确认的共识（回答「定下来什么」）
//   fact/     提炼过的结论（人为写的，不是会话遗言）
//
// 注意 fact/ 与旧模型的 fact 分支不是同一件事：
// 旧分支里装的是 Pi 会话压缩时自动生成的 Compaction summary，
// 那是「会话遗言」（待办清单 + 约束清单），不是知识，已废弃不入库。
// 新结构下的 fact/ 是手动写下的确定结论，与 thought/ 同等待遇。
//
// 不参与：me/（每轮注入的规则，不靠检索）、以及任何其他路径。
func isSemanticContent(path string) bool {
	for _, p := range []string{"thought/", "record/", "review/", "fact/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// semanticMinScore 是向量命中的最低余弦相似度。
//
// 0.35 的依据是实测分布：无关内容 0.19~0.30，跨语言正解 0.45，
// 同语言相关 0.50~0.60。取 0.35 能把无关内容挡在门外，
// 又不至于把跨语言的正确答案筛掉——那是这个功能存在的理由。
const semanticMinScore = 0.35

// semanticState 是向量侧的运行状态。
type semanticState struct {
	mu    sync.RWMutex
	ready bool
	idx   *vecindex.Index
	// emb 是常驻推理会话。模型 449MB，每条查询重新加载会把
	// 延迟从毫秒推到秒级，而且反复 InitializeEnvironment /
	// DestroyEnvironment 在并发下不可靠。
	emb embed.Embedder
	err string // 构建失败的原因，供 status 报告
	// prints 是当前 idx 的内容指纹表（id -> fingerprint(text)）。
	// 与 idx 同生共死：每次替换 idx 时一起换。后台追平用它做增量比对，
	// 避免反复从磁盘读 manifest。
	prints map[string]string

	// building 为真表示后台正在建索引。此时 srecall 回落到 BM25，
	// 而不是阻塞等——检索是热路径，不能让人等几十秒。
	building bool
}

// semanticModelName 写进 manifest：换模型必须换这个名字，
// 旧缓存会因名字对不上自动判过期重建，不会新模型查旧向量。
//
// 为什么从 bge-small-zh 换到 LaBSE：实测前者跨语言只有 0.41
// （中文问 vs 英文思考），同内容中文对中文能到 0.50——差了 0.09，
// 直接导致 P0 在全库里被压到第 22 名。LaBSE 是 Google 专为跨语言
// 句向量训练的模型，同一对文本实测 0.97。
const semanticModelName = "LaBSE-int8"

// initSemantic 启动时尝试载入向量缓存；缓存缺失或过期就后台重建。
//
// 为什么后台建：全量 3,255 篇 thought 切片算向量约需数分钟。
// 放在 New 里同步跑会让服务启动卡住几分钟，期间插件全部超时。
// 检索在构建期间回落到 BM25，可用性优先于完整性。
func (s *Server) initSemantic() {
	sc := s.cfg.Semantic
	vecDir := sc.VectorDir
	if vecDir == "" && s.dataDir != "" {
		vecDir = filepath.Join(s.dataDir, "vectors")
	}
	s.semanticDir = vecDir

	// 推理会话常驻：模型只加载一次，构建与查询共用。
	//
	// 踩过的坑：早先每条查询都 Open/Close 一遍，等于每次检索
	// 重新载入整个模型——延迟从毫秒变秒级；而且反复
	// InitializeEnvironment/DestroyEnvironment 在并发下不可靠。
	emb, err := embed.OpenChain(s.semanticChainConfig())
	if err == nil {
		emb.OnTransition(func(m string) { s.logf("%s", m) })
	}
	if err != nil {
		// 启动时全链不可用不再意味着「语义功能死刑」：
		// 维护协程会周期性重试 OpenChain，嵌入服务恢复后自动补算。
		// 这正是「写入时 Ollama 没开，开了之后自动追平」的实现路径。
		s.semantic.mu.Lock()
		s.semantic.err = fmt.Sprintf("open embedder: %v", err)
		s.semantic.mu.Unlock()
		s.logf("semantic: %s — will retry in background", s.semantic.err)
		s.startSemanticMaintainer(s.cfg.Semantic.MaintainInterval)
		return
	}
	s.semantic.mu.Lock()
	s.semantic.emb = emb
	s.semantic.mu.Unlock()
	s.logf("semantic: embedder ready (dim=%d, model=%s, providers=%v, active=%s)",
		emb.Dim(), emb.Name(), emb.Providers(), emb.ActiveLabel())

	// 先试缓存。
	//
	// 注意这里不再拿 heads 当失效判据：只要模型对、维度对，缓存就
	// 可用——差异交给增量逻辑按块比对。旧的判定是「任一分支 head
	// 变了就全废」，日常写一条记忆就要重算 6800 片 12 分钟。
	var cached *vecindex.Index
	if vecDir != "" {
		if ix, prints, err := vecindex.Load(vecDir, emb.Name()); err == nil {
			if ix.Dim() != emb.Dim() {
				// 维度对不上说明缓存是别的模型遗留的。宁可重建，
				// 也不能拿旧向量配新模型——那会静默返回错结果。
				s.logf("semantic: cache dim %d != model dim %d, rebuilding", ix.Dim(), emb.Dim())
			} else {
				cached = ix
				// 先挂上去：即使接下来要增量补算，检索也已可用，
				// 不必让 srecall 回落到 BM25 等增量跑完。
				s.semantic.mu.Lock()
				s.semantic.idx = ix
				s.semantic.prints = prints
				s.semantic.ready = true
				s.semantic.mu.Unlock()
				s.logf("semantic: loaded %d vectors from cache (%d prints)", ix.Len(), len(prints))
			}
		}
	}

	// 先出声再动工：构建要跑几分钟，日志里必须先有「我开始了」这一行，
	// 否则排查时无法区分「没启动」和「正在建」。
	if cached == nil {
		s.logf("semantic: cache miss, building index in background (dir=%s)", vecDir)
	} else {
		s.logf("semantic: checking cache for incremental updates")
	}

	// 首轮构建/追平立即跑一次（不等第一个周期）。
	go s.catchUpSemantic("startup")

	// 之后由后台维护协程持续追平：写入触发 + 周期兜底。
	s.startSemanticMaintainer(s.cfg.Semantic.MaintainInterval)
}

// semanticHeads 返回参与语义索引的分支的当前 head，用作缓存失效判据。
// treeHasSemanticContent 判断一棵树里是否包含需要语义索引的内容。
func (s *Server) treeHasSemanticContent(t *memory.Tree) bool {
	// 旧模型兼容：分支名直接叫 thought / review / thought-*
	if t.Branch == "thought" || t.Branch == "review" || strings.HasPrefix(t.Branch, "thought-") {
		return true
	}
	// 新模型：树里只要有一篇路径符合 isSemanticContent，这棵树就参与
	for _, b := range t.Indexable() {
		if isSemanticContent(b.Path) {
			return true
		}
	}
	return false
}

// semanticHeads 返回参与语义索引的每个「项目@分支」的当前 head。
func (s *Server) semanticHeads() map[string]string {
	heads := map[string]string{}
	for _, t := range s.snapshotTrees() {
		if !s.treeHasSemanticContent(t) {
			continue
		}
		repo, err := s.projects.Get(t.Project)
		if err != nil {
			continue
		}
		sha, err := repo.Resolve(t.Branch)
		if err != nil {
			continue
		}
		heads[t.Project+"@"+t.Branch] = sha
	}
	return heads
}

// buildSemanticIndex 全量重建向量索引。
//
// 复用常驻 embedder，不自己 Open：模型 449MB，建完索引后它还要
// 继续服务查询，不能在这里 Close 掉。
func (s *Server) buildSemanticIndex(vecDir string, heads map[string]string, cached *vecindex.Index, cachedPrints map[string]string) error {
	s.semantic.mu.RLock()
	emb := s.semantic.emb
	s.semantic.mu.RUnlock()
	if emb == nil {
		return fmt.Errorf("embedder not initialized")
	}
	dim := emb.Dim()

	// 收集待索引的块。只取 thought 分支，只取可读的普通内容块。
	var jobs []job
	for _, t := range s.snapshotTrees() {
		for _, b := range t.Indexable() {
			// 按**路径**决定是否进语义索引（isSemanticContent），
			// 不再看分支名。分支可以是任何名字（main、main/record、
			// task-1729），只要内容在 thought/ 或 record/ 下就参与。
			if !isSemanticContent(b.Path) {
				continue
			}
			data, rerr := s.readBlock(b)
			if rerr != nil {
				continue
			}
			text := semanticText(string(data))
			if strings.TrimSpace(text) == "" {
				continue
			}
			jobs = append(jobs, job{id: b.ID(), text: text})
		}
	}
	if len(jobs) == 0 {
		return fmt.Errorf("no thought slices found to index")
	}

	// ── 增量分拣：指纹没变的直接复用旧向量，只推理变了的 ──
	//
	// 判据是「喂给 embedder 的最终文本」的内容指纹，不是 commit head：
	// 日常写一两篇记忆，6800 片里只有那一两片的指纹会变。
	batchSize := s.cfg.Semantic.BatchSize
	if batchSize <= 0 {
		batchSize = 32
	}

	ix := vecindex.New(dim)
	prints := make(map[string]string, len(jobs))
	var toEmbed []job // 指纹变了或新增的，需要真推理
	var reused int    // 指纹没变直接复用的
	start := time.Now()

	for _, j := range jobs {
		fp := fingerprint(j.text)
		if old, had := cachedPrints[j.id]; had && old == fp && cached != nil && cached.Has(j.id) {
			// 内容没变：把旧向量原样搬进新索引，零推理开销。
			if vecs := cached.VectorOf(j.id); vecs != nil {
				if err := ix.Add(j.id, vecs); err == nil {
					prints[j.id] = fp
					reused++
					continue
				}
			}
		}
		toEmbed = append(toEmbed, j)
	}

	if cached != nil {
		s.logf("semantic: incremental: %d reused, %d changed/new (of %d)", reused, len(toEmbed), len(jobs))
	} else {
		s.logf("semantic: embedding %d thought slices (full build)", len(toEmbed))
	}

	// 批量嵌入：HTTP 后端（TEI/Ollama）靠一次请求吃多条才能真正
	// 并发起来——逐条调用会让 5,000+ 片的建索引慢一个量级。
	// ONNX 后端的 EmbedBatch 是加锁循环，行为不变。
	var failed int
	for i := 0; i < len(toEmbed); i += batchSize {
		end := i + batchSize
		if end > len(toEmbed) {
			end = len(toEmbed)
		}
		batch := toEmbed[i:end]
		batchIn := make([]string, len(batch))
		for k, j := range batch {
			batchIn[k] = j.text
		}
		vs, berr := emb.EmbedBatch(batchIn)
		if berr != nil {
			// 一整批失败不再等于整批报废。
			//
			// 实测的故障形态：后端对整批返回 413「input length exceeds
			// the context length」，于是同批另外十几条一起被计成失败，
			// 而它们单独送进去全部正常。真正的病根是「批」这个粒度：
			// 请求总量超过了后端的上下文。
			//
			// 更糟的是下一轮增量重建会重试，同样的批再次整体失败，形成
			// 「一批内容永远进不了索引」的稳态，而 status 只报数字。
			//
			// 所以整批失败后二分重试：先试一半，坏的那半继续二分，直到
			// 定位到真的嵌不进的那一片。全批都是好的时候只需 2 次请求，
			// 而不是逐条 16 次。
			s.logf("semantic: batch %d-%d failed (%v); isolating by halves", i, end, berr)
			vs = s.embedIsolate(emb, batch, &failed)
		}
		for k, v := range vs {
			if v == nil {
				// 逐条兜底里失败的那几条：failed 已经在上面记过，
				// 这里不能把空向量塞进索引（那不是「零向量」而是没有）。
				continue
			}
			if err := ix.Add(batch[k].id, v); err != nil {
				failed++
				continue
			}
			prints[batch[k].id] = fingerprint(batch[k].text)
		}
		if end%200 < batchSize {
			s.logf("semantic: %d/%d embedded (%.1fs)", end, len(toEmbed), time.Since(start).Seconds())
		}
	}
	if ix.Len() == 0 {
		return fmt.Errorf("all %d slices failed to embed", len(jobs))
	}

	if vecDir != "" {
		if err := ix.Save(vecDir, emb.Name(), heads, prints); err != nil {
			s.logf("semantic: cache save failed (will rebuild next start): %v", err)
		}
	}

	s.semantic.mu.Lock()
	s.semantic.idx = ix
	s.semantic.prints = prints
	s.semantic.ready = true
	s.semantic.mu.Unlock()

	s.logf("semantic: ready with %d vectors (%d reused, %d failed) in %.1fs",
		ix.Len(), reused, failed, time.Since(start).Seconds())
	return nil
}

// semanticText 从切片正文里剥掉 frontmatter，只留真正的推理内容。
//
// 为什么要剥：实测未剥时，同一轮对话切出的相邻片段前 512 个 token
// 几乎相同（都是 frontmatter + 引用的同一句提问），向量得分撞成
// 同一个数字，排序完全失效。frontmatter 是元数据，不是内容，
// 让它参与语义计算等于用噪声覆盖信号。
func semanticText(raw string) string {
	s := raw
	// 剥掉一层或两层 frontmatter（导入的切片有时是双层）
	for i := 0; i < 2; i++ {
		if !strings.HasPrefix(s, "---\n") {
			break
		}
		end := strings.Index(s[4:], "\n---\n")
		if end < 0 {
			break
		}
		s = s[4+end+5:]
	}
	return strings.TrimSpace(s)
}

// semanticSearch 在向量索引上查一次。返回空表示未启用或未就绪。
func (s *Server) semanticSearch(query string) []vecindex.Hit {
	s.semantic.mu.RLock()
	ix := s.semantic.idx
	emb := s.semantic.emb
	ready := s.semantic.ready
	s.semantic.mu.RUnlock()
	if !ready || ix == nil || emb == nil {
		return nil
	}

	qv, err := emb.Embed(query)
	if err != nil {
		return nil
	}
	// 扩大候选池到 30：实测 1729 案的 P0 排第 21、L2 种子排第 25，
	// 如果只取 8 个，它们在向量路全被掐掉，RRF 叠加失效。
	// 3,300 篇向量扫描只耗 2ms，取 30 个几乎没有额外开销。
	return ix.Search(qv, 30, semanticMinScore)
}

// companionRecord 找出与一条 thought 切片同轮的 record 切片。
//
// 为什么需要：命中推理链只回答了「为什么」，用户还要「结论」。
// 同一轮的两个侧面在库里是分开的：
//
//	thought/2026-10-28/.../124946.679516-1986-laptop.md  ← Hardy–Ramanujan
//	record/2026-10-28/.../124946.367782-1937-laptop.md   ← 端口约定
//
// 两者的 at: 时间戳相同（同一轮），但文件名不同、路径不同。
// 所以按「同项目 + 同日期目录 + 最近时间戳」匹配。
//
// 匹配不到就返回空——宁可只给推理链，也不拿一篇不相干的 record 冒充结论。
func (s *Server) companionRecord(thoughtID string) (string, string) {
	proj, br, path, ok := memory.SplitID(thoughtID)
	if !ok {
		return "", ""
	}
	// 按路径判断这条是不是推理链，不看分支名。
	// 新语法下推理链在 thought/ 下，分支可以是任何名字。
	if !strings.HasPrefix(path, "thought/") {
		return "", ""
	}
	// 同一条生命线里，record 与 thought 是镜像目录：
	//   main/record:thought/2026-11-04/001.md
	//   main/record:record/2026-11-04/001.md
	// 所以把路径前缀换掉就能直接算出对方地址——O(1)，不用扫全库。
	rel := strings.TrimPrefix(path, "thought/")
	sameBranchID := memory.MakeID(proj, br, "record/"+rel)

	// 第一优先：同分支镜像路径直接命中。
	if at := s.semanticTime(sameBranchID); at != 0 {
		return sameBranchID, s.addrToVirtualFor(sameBranchID)
	}

	// 第二优先：旧数据布局——thought 与 record 在不同分支、日期目录相同、
	// 时间戳相近。这条回落链只为历史数据保留，新写入不依赖它。
	atThought := s.semanticTime(thoughtID)
	if atThought == 0 {
		return "", ""
	}
	dir := path
	if i := strings.LastIndex(path, "/"); i > 0 {
		dir = path[:i]
	}

	var bestID string
	var bestDelta int64 = 1 << 62
	for _, t := range s.snapshotTrees() {
		if t.Project != proj {
			continue
		}
		for _, b := range t.Indexable() {
			if !strings.HasPrefix(b.Path, "record/") && t.Branch != "record" {
				continue
			}
			bp := b.Path
			bdir := bp
			if i := strings.LastIndex(bp, "/"); i > 0 {
				bdir = bp[:i]
			}
			if bdir != dir {
				continue
			}
			at := s.semanticTime(b.ID())
			if at == 0 {
				continue
			}
			d := at - atThought
			if d < 0 {
				d = -d
			}
			if d > 300 {
				continue
			}
			if d < bestDelta {
				bestDelta = d
				bestID = b.ID()
			}
		}
	}
	if bestID == "" {
		return "", ""
	}
	return bestID, s.addrToVirtualFor(bestID)
}

// semanticTime 查一条切片的正文时间，优先索引表，回退 commit 时间。
func (s *Server) semanticTime(id string) int64 {
	if t := s.idx.DocTime(id); t != 0 {
		return t
	}
	return 0
}

// addrToVirtualFor 把完整 ID 转成对调用方可见的虚拟地址。
// 这里不判权限（调用方已经在别处判过），只做形态转换。
func (s *Server) addrToVirtualFor(id string) string {
	proj, br, path, ok := memory.SplitID(id)
	if !ok {
		return id
	}
	if proj == "" || path == "" {
		return id
	}
	// 默认分支不写 @branch，保持地址简短
	if br == s.defaultBranch {
		return proj + "/" + path
	}
	return memory.MakeID(proj, br, path)
}

// semanticStatus 给 status 动词报告向量侧状态。
func (s *Server) semanticStatus() string {
	s.semantic.mu.RLock()
	defer s.semantic.mu.RUnlock()
	// 后端详情（降级链的当前生效应是排查「为什么召回变慢/变差」
	// 的第一入口）：不依赖锁，Chain 自己是并发安全的。
	var backend string
	if emb := s.semantic.emb; emb != nil {
		if ch, ok := emb.(*embed.Chain); ok {
			backend = " via " + ch.ActiveLabel()
			// 只在「当前确实在用备用后端」时报降级原因。
			//
			// 早先无条件附加 LastError()，而那个值从不被清除，于是出现过
			// 「ready (7877 vectors) via local-ollama [all providers failed]」
			// 这种自相矛盾的输出——用户无法判断该信哪半句。
			// 现在的规则：降级=现在的事实（报），恢复后=历史（不报）。
			if ch.Degraded() {
				if msg, at, ok := ch.Note(); ok {
					backend += fmt.Sprintf(" [DEGRADED %s ago: %s]",
						time.Since(at).Truncate(time.Second), msg)
				} else {
					backend += " [degraded]"
				}
			}
		}
	}
	switch {
	case s.semantic.building:
		return "building" + backend
	case s.semantic.ready && s.semantic.idx != nil:
		return fmt.Sprintf("ready (%d vectors)%s", s.semantic.idx.Len(), backend)
	case s.semantic.err != "":
		return "unavailable: " + s.semantic.err
	default:
		return "not initialized"
	}
}

// snapshotTrees 取当前树的快照（调用方已持有锁时不要用）。
func (s *Server) snapshotTrees() []*memory.Tree {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*memory.Tree, len(s.trees))
	copy(out, s.trees)
	return out
}

// contentHash 给切片内容算个短指纹，用于增量更新判断。
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

var _ = os.Getenv // 保留 os 引用，供后续按环境变量覆盖路径

// semanticChainConfig 把服务配置与环境变量组装成降级链。
//
// 优先级（高到低）：
//  1. 环境变量 ZERO_MEMCP_EMBED_ENDPOINT（直接插到链首，便于容器和快速测试）
//  2. 配置文件里的 [[semantic.provider]] 链
//  3. 单后端字段 [semantic] lib/model/vocab（向前兼容）
func (s *Server) semanticChainConfig() embed.ChainConfig {
	sc := s.cfg.Semantic
	var providers []embed.ProviderConfig

	// 1. 环境变量直插链首
	if ep := os.Getenv("ZERO_MEMCP_EMBED_ENDPOINT"); ep != "" {
		model := os.Getenv("ZERO_MEMCP_EMBED_MODEL")
		providers = append(providers, embed.ProviderConfig{
			Label:    "env-http",
			Kind:     "http",
			Endpoint: ep,
			Model:    model,
			APIKey:   os.Getenv("ZERO_MEMCP_EMBED_APIKEY"),
			Timeout:  5 * time.Second,
		})
	}

	// 2. 配置里的 [[semantic.provider]]
	for _, p := range s.fileSemanticProviders() {
		providers = append(providers, embed.ProviderConfig{
			Label:     p.Label,
			Kind:      p.Kind,
			Endpoint:  p.Endpoint,
			Model:     p.Model,
			Dim:       p.Dim,
			APIKey:    p.APIKey,
			Timeout:   p.Timeout,
			LibPath:   p.LibPath,
			ModelPath: p.ModelPath,
			VocabPath: p.VocabPath,
		})
	}

	// 3. 旧单后端兼容（如果链还是空的）
	if len(providers) == 0 {
		lib := sc.LibPath
		if envLib := os.Getenv("ZERO_MEMCP_ORT_LIB"); envLib != "" {
			lib = envLib
		}
		if lib != "" && sc.ModelPath != "" && sc.VocabPath != "" {
			providers = append(providers, embed.ProviderConfig{
				Label:     "local-onnx",
				Kind:      "onnx",
				LibPath:   lib,
				ModelPath: sc.ModelPath,
				VocabPath: sc.VocabPath,
			})
		}
	}

	return embed.ChainConfig{Providers: providers}
}

// fileSemanticProviders 读配置里的 provider 列表。
func (s *Server) fileSemanticProviders() []config.SemanticProvider {
	return s.cfg.Semantic.Providers
}

// fingerprint 是一块文本的内容指纹（语义索引的增量判据）。
//
// 与 contentHash 不同之处：contentHash 服务于别名去重（8 字节够用），
// 这里是唯一的数据正确性判据——指纹撞了就会错拿旧向量，所以用全长
// SHA-256。碰撞概率 2^-256，比磁盘位翻转还低。
func fingerprint(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// job 是一条待嵌入的切片。
//
// 提到包级是为了让二分隔离（embedIsolate）能接收整批——
// 它原本是 buildSemanticIndex 里的局部类型。
type job struct {
	id   string
	text string
}

// maxEmbedChars 是送进嵌入模型前允许的最大字符数。
//
// bge-m3 的上下文是 8192 token；中日韩字符大致一个 token 一个字，
// 标点与数字会多一些。取 6000 是留够余量后的安全线。
//
// 这个数字不需要精确——它不是分词器，只是「明显超长就先截断再试」
// 的门槛。真正的长度判定由后端返回的 413 给出，这里只负责别让一条
// 几万字的文档把整次重建卡在这个坑里反复重试。
const maxEmbedChars = 6000

// embedOneSlice 嵌入一片文本：先按原文，超长再截断重试。
//
// 返回 nil 向量表示这片确实嵌不进去（调用方记失败），语义上不等于
// 「零向量」——零向量是一个合法结果，nil 不是。
func (s *Server) embedOneSlice(emb embed.Embedder, text, id string) ([]float32, error) {
	v, err := emb.Embed(text)
	if err == nil && v != nil {
		return v, nil
	}
	n := len([]rune(text))
	if n <= maxEmbedChars {
		if err == nil {
			err = fmt.Errorf("backend returned an empty vector")
		}
		return nil, err
	}
	s.logf("semantic: slice %s is %d chars, over the embedding context; truncating to %d and retrying",
		id, n, maxEmbedChars)
	v2, err2 := emb.Embed(truncateRunes(text, maxEmbedChars))
	if err2 != nil {
		return nil, err2
	}
	if v2 == nil {
		return nil, fmt.Errorf("backend returned an empty vector after truncation")
	}
	return v2, nil
}

// truncateRunes 按字符截断（不是字节），保证不切断 UTF-8 序列。
//
// 按字节截断会产生非法 UTF-8，而非法 UTF-8 在 JSON 编码时会被替换成
// U+FFFD，送到后端就是一段看似正常、实则被悄悄改过的文本——错得
// 无声无息。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// embedIsolate 在整批失败后二分重试，把能嵌的救回来。
//
// 返回长度与 jobs 等长的向量切片；嵌不进去的位置是 nil（调用方跳过并
// 计失败）。为什么二分而不是逐条：整批失败最常见的原因是「请求总量
// 超限」，此时绝大多数条目本身没问题，二分两三次就能把好的一半整体
// 送进去；逐条重试会把 16 条好内容变成 16 次往返。
func (s *Server) embedIsolate(emb embed.Embedder, jobs []job, failed *int) [][]float32 {
	out := make([][]float32, len(jobs))
	if len(jobs) == 0 {
		return out
	}
	texts := make([]string, len(jobs))
	for k := range jobs {
		texts[k] = jobs[k].text
	}
	vs, err := emb.EmbedBatch(texts)
	if err == nil {
		copy(out, vs)
		return out
	}
	if len(jobs) == 1 {
		// 到单片了还失败：可能是这一片真的超长，先截断再试一次。
		v, e := s.embedOneSlice(emb, jobs[0].text, jobs[0].id)
		if e != nil {
			*failed++
			s.logf("semantic: slice %s cannot be embedded: %v", jobs[0].id, e)
			return out
		}
		out[0] = v
		return out
	}
	half := len(jobs) / 2
	left := s.embedIsolate(emb, jobs[:half], failed)
	right := s.embedIsolate(emb, jobs[half:], failed)
	copy(out, left)
	copy(out[half:], right)
	return out
}
