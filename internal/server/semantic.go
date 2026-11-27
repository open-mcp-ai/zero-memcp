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
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
//
//	thought/  推理链（回答「为什么」）
//	record/   一问一答（回答「发生过什么」）
//	review/   人工确认的共识（回答「定下来什么」）
//	fact/     提炼过的结论（人为写的，不是会话遗言）
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

// semanticLayers 是「内容自己声明的层」里，允许进语义索引的那些。
//
// 和路径白名单是**两个入口、同一个结果**：路径对了算进，
// 内容自己写明是这几层之一也算进。
var semanticLayers = map[string]bool{
	"thought": true,
	"record":  true,
	"review":  true,
	"fact":    true,
}

// isSemanticBlock 判断一个块该不该参与语义索引。
//
// 两条入口，满足任一即可：
//
//  1. **路径**在前缀白名单里（thought/ record/ review/ fact/）——主路径
//  2. **内容自己声明了** layer: thought|record|review|fact —— 历史数据补票
//
// # 为什么第 2 条不能省
//
// 2026-11-16 实测：`docs 28187 / semantic 16167`，差 12,020 篇。
// 缺的全是历史思维链和对话——它们躺在日期根目录（`2026-11-15/xxx.md`），
// 而当时只认路径前缀，于是这些块**一个向量都没算过**。
//
// 症状极难查：数据在、能 grep、能 read，就是语义搜不到。
// 看起来像「没落盘」，实际是「落了但索引瞎了」——
// 这两件事对用户是完全不同的说法（前者是数据丢了，后者是数据还在）。
//
// 「写错路径」是**已经发生过的事实**，不是可能发生的事。
// 所以这不是提前加的防护，是补一个已经漏掉的口子。
//
// 排除项优先：me/（每轮注入的规则，不靠检索）和点开头的隐藏内容
// 一律不进，哪怕正文里声明了 layer。
func isSemanticBlock(b memory.Block) bool {
	if b.Hidden || memory.HiddenPath(b.Path) {
		return false
	}
	// me/ 装的是每轮注入系统提示的规则，靠注入不靠检索。
	// 把它放进向量库只会让 recall 里全是规则条文，挤掉真正的内容。
	if strings.HasPrefix(b.Path, "me/") || strings.Contains(b.Path, "/me/") {
		return false
	}
	if isSemanticContent(b.Path) {
		return true
	}
	layer := b.Layer
	// 如果扫描阶段没填 Layer 但 Body 已经带在身上（比如测试造的数据、
	// 或某些直接构造 Block 的快路径），顺手代为解析一次。
	// 这不引入读盘：Body 已经在内存里。
	if layer == "" && len(b.Body) > 0 {
		layer = memory.DeclaredLayer(string(b.Body))
	}
	return semanticLayers[layer]
}

// semanticMinScore 是向量命中的最低余弦相似度。
//
// 0.35 的依据是实测分布：无关内容 0.19~0.30，跨语言正解 0.45，
// 同语言相关 0.50~0.60。取 0.35 能把无关内容挡在门外，
// 又不至于把跨语言的正确答案筛掉——那是这个功能存在的理由。
const semanticMinScore = 0.35

// semanticSnapshot 是 (idx, prints) 的原子快照。
//
// 它们必须同生共死：每次替换 idx 时一起换，读者 Load 一次就拿到
// 匹配的一对，不会读到「索引是新的、指纹还是旧的」导致增量判定全部失效。
type semanticSnapshot struct {
	idx    *vecindex.Index
	prints map[string]string
}

// semanticState 是向量侧的运行状态。
//
// # 为什么没有锁
//
// 拆成三个维度的无锁原语：
//   - active：(idx, prints) 的原子快照，整体替换；
//   - emb / err：atomic.Pointer；
//   - quarantine / blobHashes：sync.Map，每个 id 互相独立；
//   - building：atomic.Bool（CAS 去重）。
//
// 读者（srecall 热路径）全程只做 Load，永不阻塞后台构建；
// 后台构建（几十秒网络推理）在锁外跑完，最后一次 Store 换上去。
type semanticState struct {
	emb        atomic.Pointer[embedHolder]
	active     atomic.Pointer[semanticSnapshot]
	err        atomic.Pointer[string]
	building   atomic.Bool
	quarantine sync.Map // id -> quarantineEntry
	blobHashes sync.Map // id -> git blob hash
}

type embedHolder struct{ emb embed.Embedder }

func (st *semanticState) getEmb() embed.Embedder {
	if h := st.emb.Load(); h != nil {
		return h.emb
	}
	return nil
}

func (st *semanticState) setEmb(emb embed.Embedder) {
	st.emb.Store(&embedHolder{emb: emb})
}

func (st *semanticState) getActive() *semanticSnapshot {
	return st.active.Load()
}

func (st *semanticState) setActive(idx *vecindex.Index, prints map[string]string) {
	st.active.Store(&semanticSnapshot{idx: idx, prints: prints})
}

func (st *semanticState) getErr() string {
	if p := st.err.Load(); p != nil {
		return *p
	}
	return ""
}

func (st *semanticState) setErr(err string) {
	st.err.Store(&err)
}

func (st *semanticState) isReady() bool {
	snap := st.getActive()
	return snap != nil && snap.idx != nil && st.getEmb() != nil
}

// quarantineEntry 是一条失败切片的退避状态。
//
// nextTry 之前不再尝试；failures 决定退避长度（指数，封顶 2 小时）。
// 不设上限的话，一条真正碎的切片会让日志和 CPU 永久背账。
type quarantineEntry struct {
	failures int
	nextTry  time.Time
	lastErr  string
}

// semanticModelName 写进 manifest：换模型必须换这个名字，
// 旧缓存会因名字对不上自动判过期重建，不会新模型查旧向量。
//
// 为什么从 bge-small-zh 换到 LaBSE：实测前者跨语言只有 0.41
// （中文问 vs 英文思考），同内容中文对中文能到 0.50——差了 0.09，
// 直接导致 P0 在全库里被压到第 22 名。LaBSE 是 Google 专为跨语言
// 句向量训练的模型，同一对文本实测 0.97。
const semanticModelName = "LaBSE-int8"

// vectorDirFor 返回某个模型专属的向量落盘目录。
//
// 多索引共存靠这一层子目录实现：远端大模型、本地小模型、同名的不同
// 修订各占据一个目录，切换时互不覆盖。于是「大模型挂了切小模型、
// 恢复后切回来」不再需要重算大模型那份——这是降级方案能成立的前提。
//
// 手动 index_dir 优先（供运维显式指定），否则由 Model+ModelRev 推导。
func (s *Server) vectorDirFor(emb embed.Embedder) string {
	if s.semanticDir == "" {
		return ""
	}
	if p := s.semanticProviderFor(emb.Name()); p != nil && p.IndexDir != "" {
		return filepath.Join(s.semanticDir, p.IndexDir)
	}
	return vecindex.ResolveDir(s.semanticDir, emb.Name(), embed.Revision(emb))
}

// semanticProviderFor 在配置里找与模型名匹配的 provider（用于取 index_dir、
// gate_threshold 这类「随模型走」的参数）。
//
// 找不到返回 nil：链的名字可能由多个 provider 拼成，此时用默认推导。
func (s *Server) semanticProviderFor(modelName string) *config.SemanticProvider {
	for i := range s.cfg.Semantic.Providers {
		p := &s.cfg.Semantic.Providers[i]
		if p.Model == modelName || p.Label == modelName {
			return p
		}
	}
	return nil
}

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
		s.semantic.setErr(fmt.Sprintf("open embedder: %v", err))
		s.logf("semantic: %s — will retry in background", s.semantic.getErr())
		s.startSemanticMaintainer(s.cfg.Semantic.MaintainInterval)
		return
	}
	s.semantic.setEmb(emb)
	s.logf("semantic: embedder ready (dim=%d, model=%s, providers=%v, active=%s)",
		emb.Dim(), emb.Name(), emb.Providers(), emb.ActiveLabel())

	// 先试缓存。
	//
	// 注意这里不再拿 heads 当失效判据：只要模型对、维度对，缓存就
	// 可用——差异交给增量逻辑按块比对。旧的判定是「任一分支 head
	// 变了就全废」，日常写一条记忆就要重算 6800 片 12 分钟。
	var cached *vecindex.Index
	modelDir := s.vectorDirFor(emb)
	if modelDir != "" {
		if ix, prints, err := vecindex.LoadRev(modelDir, emb.Name(), embed.Revision(emb)); err == nil {
			if ix.Dim() != emb.Dim() {
				// 维度对不上说明缓存是别的模型遗留的。宁可重建，
				// 也不能拿旧向量配新模型——那会静默返回错结果。
				s.logf("semantic: cache dim %d != model dim %d, rebuilding", ix.Dim(), emb.Dim())
			} else {
				cached = ix
				// 先挂上去：即使接下来要增量补算，检索也已可用，
				// 不必让 srecall 回落到 BM25 等增量跑完。
				s.semantic.setActive(ix, prints)
				s.logf("semantic: loaded %d vectors from cache (%d prints, dir=%s)",
					ix.Len(), len(prints), modelDir)
			}
		} else {
			// 旧版本把 vectors.bin 直接写在根目录。若那里恰好是本模型
			// 且修订号为空（老 manifest 没有这个字段），继续沿用，
			// 下次落盘时自动搬进模型子目录，不必手工搬文件。
			if ix, prints, lerr := vecindex.Load(vecDir, emb.Name()); lerr == nil && ix.Dim() == emb.Dim() {
				cached = ix
				s.semantic.setActive(ix, prints)
				s.logf("semantic: adopting legacy cache at %s (%d vectors)", vecDir, ix.Len())
			}
		}
	}

	// 先出声再动工：构建要跑几分钟，日志里必须先有「我开始了」这一行，
	// 否则排查时无法区分「没启动」和「正在建」。
	if cached == nil {
		s.logf("semantic: cache miss, building index in background (dir=%s)", modelDir)
	} else {
		s.logf("semantic: checking cache for incremental updates")
	}

	// 首轮构建/追平立即跑一次（不等第一个周期）。
	go s.catchUpSemantic("startup")

	// 若配置里写了多个不同模型（如 zh + en），把其余已建好的索引也挂上，
	// 形成真正的多索引并存，查询时各路并查再按名次融合。
	s.loadLiveIndexes()

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
	emb := s.semantic.getEmb()
	if emb == nil {
		return fmt.Errorf("embedder not initialized")
	}
	dim := emb.Dim()

	// 收集待索引的块。只取 thought 分支，只取可读的普通内容块。
	var jobs []job
	for _, t := range s.snapshotTrees() {
		if !s.branchIndexed(t.Project, t.Branch) {
			continue
		}
		for _, b := range t.Indexable() {
			// 按**路径**决定是否进语义索引（isSemanticContent），
			// 不再看分支名。分支可以是任何名字（main、main/record、
			// task-1729），只要内容在 thought/ 或 record/ 下就参与。
			if !isSemanticBlock(b) {
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

	// 按内容指纹反查旧向量：指纹 → 任意一个曾有该内容的旧地址。
	//
	// 为什么需要这张表：旧向量按「地址」存，而地址里带着路径。文件一改名
	// （规整分桶、挪出白名单、换分支前缀），地址就变了，按地址查必然 miss——
	// 于是 3 万篇没改一个字的记忆会被当成新文档全部重算嵌入，代价是几十分钟
	// 的 Ollama 排队。
	//
	// 但向量只取决于**内容**。同一段文字放在哪个路径，向量完全一样。所以
	// 地址没命中、指纹命中时，把那份向量原样借给新地址。改名因此变成纯内存
	// 操作，零推理。
	//
	// 为什么不干脆把主键换成 blob 哈希：哈希确实更纯（同内容天然共享一份
	// 向量），但它会让权限过滤、时间加成、思维↔记录配对、虚拟地址这四处
	// 全部改依赖「哈希 → 地址」翻译层，改动面远大于收益；而「改路径不重算」
	// 这个目标，指纹反查已经 100% 达成。
	byPrint := make(map[string]string, len(cachedPrints))
	if cached != nil {
		for id, fp := range cachedPrints {
			if _, ok := byPrint[fp]; ok {
				continue
			}
			if cached.Has(id) {
				byPrint[fp] = id
			}
		}
	}
	var reusedByPrint int

	// 隔离检查：退避期内的切片直接跳过，不参与本轮。
	//
	// 判据只看「时间到没到」，不看重试次数——退避长度本身已经由
	// 次数算出来了。跳过的切片不会进 prints，所以它仍被算作「欠着」，
	// 时间一到就会自动重试：既不会永久放弃，也不会每轮都撞。
	now := time.Now()
	var skipped int
	for _, j := range jobs {
		if v, ok := s.semantic.quarantine.Load(j.id); ok {
			e := v.(quarantineEntry)
			if e.nextTry.After(now) {
				skipped++
			}
		}
	}
	if skipped > 0 {
		s.logf("semantic: %d slice(s) in backoff, skipped this round", skipped)
	}

	for _, j := range jobs {
		if v, ok := s.semantic.quarantine.Load(j.id); ok {
			if e, _ := v.(quarantineEntry); e.nextTry.After(now) {
				continue
			}
		}
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
		// 地址换了、内容没换：向旧地址借这份向量，同样零推理。
		// 这一支就是「规整化 / 改名 / 换分支」不重算的根据。
		if cached != nil {
			if oldID, ok := byPrint[fp]; ok {
				if vecs := cached.VectorOf(oldID); vecs != nil {
					if err := ix.Add(j.id, vecs); err == nil {
						prints[j.id] = fp
						reused++
						reusedByPrint++
						continue
					}
				}
			}
		}
		toEmbed = append(toEmbed, j)
	}

	if reusedByPrint > 0 {
		s.logf("semantic: %d slice(s) reused across renamed paths (same content, new address)", reusedByPrint)
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
		if berr != nil && transientEmbedError(berr.Error()) {
			// 后端整体不可用：整轮放弃，不做二分隔离。
			//
			// 为什么不能继续二分：二分会把「后端连不上」逐条判成
			// 「这一片有问题」。一个 8,826 片的库会因此发出上万次
			// 注定失败的请求，每次都要等超时——一轮维护能跑十几分钟，
			// 而且把这些片全打进 quarantine。
			//
			// 正确做法是把这一轮当作「没跑过」：不记失败、不动退避，
			// 下一轮（几十秒后）再试。后端恢复后自然就补上了。
			return fmt.Errorf("embedding backend unavailable, aborting this round: %w", berr)
		}
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
				s.noteSemanticFailure(batch[k].id, "embedding failed")
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

	if saveDir := s.vectorDirFor(emb); saveDir != "" {
		if err := ix.SaveRev(saveDir, emb.Name(), embed.Revision(emb), heads, prints); err != nil {
			s.logf("semantic: cache save failed (will rebuild next start): %v", err)
		}
	}

	// 索引结果另开一棵树（<project>@meta.index），不碰主记录 blob。
	// 好处：主历史保持干净，而排错需要的「哪版权重、何时、覆盖了哪些
	// 切片」全在这棵树上，且自带 git diff；写完顺手通知订阅者。
	s.publishIndexTree(emb.Name(), embed.Revision(emb), emb.Dim(), prints)

	s.semantic.setActive(ix, prints)

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
	snap := s.semantic.getActive()
	emb := s.semantic.getEmb()
	if snap == nil || snap.idx == nil || emb == nil {
		return nil
	}
	ix := snap.idx

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
	if t := s.idxNow().DocTime(id); t != 0 {
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
	snap := s.semantic.getActive()
	var backend string
	if emb := s.semantic.getEmb(); emb != nil {
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
	case s.semantic.building.Load():
		return "building" + backend
	case snap != nil && snap.idx != nil:
		return fmt.Sprintf("ready (%d vectors)%s", snap.idx.Len(), backend)
	case s.semantic.getErr() != "":
		return "unavailable: " + s.semantic.getErr()
	default:
		return "not initialized"
	}
}

// snapshotTrees 取当前树的快照（调用方已持有锁时不要用）。
func (s *Server) snapshotTrees() []*memory.Tree {
	out := make([]*memory.Tree, len(s.treeList()))
	copy(out, s.treeList())
	return out
}

// contentHash 给切片内容算个短指纹，用于增量更新判断。
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

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
			ModelRev:  p.ModelRev,
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
// 这个数字不需要精确——它不是分词器，只是「明显超长就切块」的门槛。
// 真正的长度判定由后端返回的 413 给出。
const maxEmbedChars = 6000

// embedChunkChars 是长文档切块的块大小。
//
// 为什么要切而不是截断：截断只保留开头，一篇几万字的记录后半段
// 永远进不了向量——「写了但搜不到」，而且没有任何报错。
// 切块后每块单独嵌入、取均值再归一化，得到的是整篇的语义中心。
//
// 1500 是折中的产物：小块更贴近局部语义，但块数多、请求多；
// 大块接近上限反而容易在后端触发上下文超限。取 1500 时，
// 6000 字的文档是 4 块，一次批量请求就能送完。
const embedChunkChars = 1500

// embedOneSlice 嵌入一片文本：短文档直嵌，长文档切块后取语义中心。
//
// 返回 nil 向量表示这片确实嵌不进去（调用方记失败并进退避队列），
// 语义上不等于「零向量」——零向量是一个合法结果，nil 不是。
func (s *Server) embedOneSlice(emb embed.Embedder, text, id string) ([]float32, error) {
	runes := []rune(text)
	if len(runes) <= maxEmbedChars {
		v, err := emb.Embed(text)
		if err == nil && v != nil {
			return v, nil
		}
		if err == nil {
			err = fmt.Errorf("backend returned an empty vector")
		}
		// 短文档也可能因为后端上下文更小而超限：截半再试一次。
		half := truncateRunes(text, len(runes)/2)
		if half == text || half == "" {
			return nil, err
		}
		s.logf("semantic: slice %s is %d chars and failed (%v); retrying with the first half", id, len(runes), err)
		if v2, err2 := emb.Embed(half); err2 == nil && v2 != nil {
			return v2, nil
		}
		return nil, err
	}

	// 长文档：切块、逐块嵌入、取均值。
	chunks := splitRunes(runes, embedChunkChars)
	s.logf("semantic: slice %s is %d chars, over the embedding window; embedding %d chunks of %d and averaging",
		id, len(runes), len(chunks), embedChunkChars)

	vecs, err := emb.EmbedBatch(chunks)
	if err != nil || len(vecs) != len(chunks) {
		// 批量失败时逐块退让：只要还有块能嵌进去，就别丢掉整篇。
		vecs = make([][]float32, len(chunks))
		var firstErr error = err
		ok := 0
		for i, ch := range chunks {
			v, verr := emb.Embed(ch)
			if verr != nil || v == nil {
				if firstErr == nil {
					firstErr = verr
				}
				continue
			}
			vecs[i] = v
			ok++
		}
		if ok == 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("all %d chunks failed to embed", len(chunks))
			}
			return nil, firstErr
		}
	}
	return meanVector(vecs), nil
}

// splitRunes 把 rune 序列切成不超过 n 的连续块。
func splitRunes(r []rune, n int) []string {
	if n <= 0 {
		n = 1
	}
	var out []string
	for start := 0; start < len(r); start += n {
		end := start + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[start:end]))
	}
	return out
}

// meanVector 对多块向量取均值并做 L2 归一化。
//
// 归一化不能省：下游按余弦相似度打分，均值后的向量模长会小于 1，
// 不归一会让长文档在同一个阈值下被系统性压低或抬高——
// 排序结果取决于文档长短，而不是内容相关度。
func meanVector(vecs [][]float32) []float32 {
	var acc []float32
	n := 0
	for _, v := range vecs {
		if v == nil {
			continue
		}
		if acc == nil {
			acc = make([]float32, len(v))
		}
		if len(v) != len(acc) {
			continue
		}
		for i, x := range v {
			acc[i] += x
		}
		n++
	}
	if acc == nil || n == 0 {
		return nil
	}
	var sum float64
	for i := range acc {
		acc[i] /= float32(n)
		sum += float64(acc[i] * acc[i])
	}
	if sum > 0 {
		norm := float32(math.Sqrt(sum))
		for i := range acc {
			acc[i] /= norm
		}
	}
	return acc
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
			s.noteSemanticFailure(jobs[0].id, e.Error())
			s.logf("semantic: slice %s cannot be embedded (backing off): %v", jobs[0].id, e)
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

// transientEmbedError 判断一次嵌入失败是「环境不可用」还是「这片内容有问题」。
//
// 这个区分是关键。早先一律按「这片有问题」退避，于是：
//   - 3060 断网 30 秒
//   - 二分隔离把整批的每一条都判成「单独送进去也失败」
//   - 每条各记一次失败，退避 1 分钟；下一轮再失败，退避 5 分钟
//   - 几轮之后 30 分钟、2 小时
//
// 结果是「服务早就恢复了，但索引要等两小时才补」，
// 而 status 只报数字，没人看得出是被退避卡住的。
//
// 环境不可用时**不该**退避：它跟这片内容无关，下一轮就该重试。
func transientEmbedError(reason string) bool {
	r := strings.ToLower(reason)
	for _, sig := range []string{
		"all providers failed", // 整条降级链都连不上
		"connection refused",   // 后端没起来
		"no such host",         // DNS 解析不了
		"service unavailable",  // 503
		"timeout",              // 超时（含 context deadline exceeded）
		"deadline exceeded",
		"connection reset",
		"broken pipe",
		"eof", // 对端提前关闭
	} {
		if strings.Contains(r, sig) {
			return true
		}
	}
	return false
}

// noteSemanticFailure 记一次嵌入失败，并算出下次允许重试的时间。
//
// 退避按 1m、5m、30m、2h 递增并封顶：第一次失败多半是后端抖动，
// 一分钟后重试就够；一直失败说明这片内容本身有问题（超长、二进制、
// 编码坏），那就别让它在每轮维护里反复撞——每撞一次都要二分隔离
// 一整批，代价远大于它自己的价值。
//
// 但「环境不可用」不走退避，见 transientEmbedError 的说明。
func (s *Server) noteSemanticFailure(id, reason string) {
	if transientEmbedError(reason) {
		s.semantic.quarantine.Delete(id)
		return
	}

	const (
		base = time.Minute
		max  = 2 * time.Hour
	)
	var e quarantineEntry
	if v, ok := s.semantic.quarantine.Load(id); ok {
		e = v.(quarantineEntry)
	}
	e.failures++
	e.lastErr = reason
	back := base
	for i := 1; i < e.failures && back < max; i++ {
		back *= 5
	}
	if back > max {
		back = max
	}
	e.nextTry = time.Now().Add(back)
	s.semantic.quarantine.Store(id, e)
}

// semanticRouteLabel 说明本轮语义检索实际走了哪一路。
//
// 它回答的是「这条分数是谁给的」：同一个查询在不同模型下分数不同，
// 不回显模型就无从比较、也无从怀疑。
func (s *Server) semanticRouteLabel() string {
	if lbl := s.liveIndexLabel(); lbl != "" {
		return lbl
	}
	if !s.semantic.isReady() {
		return "none (lexical only)"
	}
	if emb := s.semantic.getEmb(); emb != nil {
		return emb.Name()
	}
	return "unknown"
}
