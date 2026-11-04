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
	emb *embed.Embedder
	err string // 构建失败的原因，供 status 报告

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
	if sc.LibPath == "" || sc.ModelPath == "" || sc.VocabPath == "" {
		s.semantic.mu.Lock()
		s.semantic.err = "semantic search disabled (lib/model/vocab not configured)"
		s.semantic.mu.Unlock()
		// 这里必须出声：静默返回就是「看起来在工作、实际没跑」的那个病。
		// 排查的人需要能从日志直接分辨「没配置」和「配了但在建」。
		s.logf("semantic: disabled (%s)", s.semantic.err)
		return
	}

	vecDir := sc.VectorDir
	if vecDir == "" && s.dataDir != "" {
		vecDir = filepath.Join(s.dataDir, "vectors")
	}
	s.semanticDir = vecDir

	// 推理会话常驻：449MB 模型只加载一次，构建与查询共用。
	//
	// 踩过的坑：早先每条查询都 Open/Close 一遍，等于每次检索
	// 重新载入 449MB——延迟从毫秒变秒级；而且反复
	// InitializeEnvironment/DestroyEnvironment 在并发下不可靠。
	emb, err := embed.Open(embed.Config{
		LibPath:   sc.LibPath,
		ModelPath: sc.ModelPath,
		VocabPath: sc.VocabPath,
	})
	if err != nil {
		s.semantic.mu.Lock()
		s.semantic.err = fmt.Sprintf("open embedder: %v", err)
		s.semantic.mu.Unlock()
		s.logf("semantic: %s — disabled", s.semantic.err)
		return
	}
	s.semantic.mu.Lock()
	s.semantic.emb = emb
	s.semantic.mu.Unlock()
	s.logf("semantic: embedder ready (dim=%d, model=%s)", emb.Dim(), semanticModelName)

	heads := s.semanticHeads()

	// 先试缓存
	if vecDir != "" {
		if ix, err := vecindex.Load(vecDir, semanticModelName, heads); err == nil {
			if ix.Dim() != emb.Dim() {
				// 维度对不上说明缓存是别的模型遗留的。宁可重建，
				// 也不能拿旧向量配新模型——那会静默返回错结果。
				s.logf("semantic: cache dim %d != model dim %d, rebuilding", ix.Dim(), emb.Dim())
			} else {
				s.semantic.mu.Lock()
				s.semantic.idx = ix
				s.semantic.ready = true
				s.semantic.mu.Unlock()
				s.logf("semantic: loaded %d vectors from cache", ix.Len())
				return
			}
		}
	}

	s.semantic.mu.Lock()
	s.semantic.building = true
	s.semantic.mu.Unlock()

	// 先出声再动工：构建要跑几分钟，日志里必须先有「我开始了」这一行，
	// 否则排查时无法区分「没启动」和「正在建」。
	s.logf("semantic: cache miss, building index in background (dir=%s)", vecDir)

	go func() {
		if err := s.buildSemanticIndex(vecDir, heads); err != nil {
			s.semantic.mu.Lock()
			s.semantic.err = err.Error()
			s.semantic.building = false
			s.semantic.mu.Unlock()
			s.logf("semantic: build failed: %v", err)
			return
		}
		s.semantic.mu.Lock()
		s.semantic.building = false
		s.semantic.mu.Unlock()
	}()
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
func (s *Server) buildSemanticIndex(vecDir string, heads map[string]string) error {
	s.semantic.mu.RLock()
	emb := s.semantic.emb
	s.semantic.mu.RUnlock()
	if emb == nil {
		return fmt.Errorf("embedder not initialized")
	}
	dim := emb.Dim()

	// 收集待索引的块。只取 thought 分支，只取可读的普通内容块。
	type job struct {
		id   string
		text string
	}
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
	s.logf("semantic: embedding %d thought slices", len(jobs))

	ix := vecindex.New(dim)
	start := time.Now()
	var failed int
	for i, j := range jobs {
		v, eerr := emb.Embed(j.text)
		if eerr != nil {
			failed++
			continue
		}
		if err := ix.Add(j.id, v); err != nil {
			failed++
		}
		if (i+1)%200 == 0 {
			s.logf("semantic: %d/%d embedded (%.1fs)", i+1, len(jobs), time.Since(start).Seconds())
		}
	}
	if ix.Len() == 0 {
		return fmt.Errorf("all %d slices failed to embed", len(jobs))
	}

	if vecDir != "" {
		if err := ix.Save(vecDir, semanticModelName, heads); err != nil {
			s.logf("semantic: cache save failed (will rebuild next start): %v", err)
		}
	}

	s.semantic.mu.Lock()
	s.semantic.idx = ix
	s.semantic.ready = true
	s.semantic.mu.Unlock()

	s.logf("semantic: ready with %d vectors (%d failed) in %.1fs",
		ix.Len(), failed, time.Since(start).Seconds())
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
	switch {
	case s.semantic.building:
		return "building"
	case s.semantic.ready && s.semantic.idx != nil:
		return fmt.Sprintf("ready (%d vectors)", s.semantic.idx.Len())
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
