package server

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
	"github.com/open-mcp-ai/zero-memcp/internal/vecindex"
)

// indexTreeBranch 是存放「索引结果清单」的专用分支。
//
// 为什么单开分支，而不是写进主记录的 blob 里：
//
//	① 主记录内容必须与模型无关。把 model_name 塞进记忆正文，换一次模型
//	   等于 38,000 个 blob 全部变脸，同步时立刻判定为全库冲突。
//	② 单开一条 tree，主历史依然干干净净；而当你要排错时——「这条切片
//	   到底是用哪版权重、哪天算出来的、指纹是什么」——这棵树里有全部
//	   答案，且自带 diff，不需自造一套 log 格式。
//	③ 写完触发一次 publishTreeChange，订阅了这棵树的 Agent 上下文会
//	   异步收到通知：「某模型的索引刚刚追平」。
const indexTreeBranch = "meta.index"

// indexTreeSnapshot 是写进 <project>@meta.index:<modelKey>.json 的结构。
type indexTreeSnapshot struct {
	Model   string    `json:"model"`
	Rev     string    `json:"model_rev,omitempty"`
	Dim     int       `json:"dim"`
	Count   int       `json:"count"`
	BuiltAt time.Time `json:"built_at"`
	// Slices 是「树内块地址 -> 喂给嵌入器的内容指纹」。
	// 不放 float32 矩阵本身：几十 MB 浮点进 Git 会让 repack 爆炸，
	// 而「哪个模型在什么时候覆盖了哪些切片」靠指纹就足够排错。
	Slices map[string]string `json:"slices"`
}

// publishIndexTree 把本轮建好的向量索引摘要，按项目写入各自的 meta.index 分支，
// 并向订阅者异步投递通知。
//
// 全程进工作层（不强制 commit），于是不会往历史里刷一堆「index updated」噪声，
// 同时工作层同步会把它一起带去其他节点。
func (s *Server) publishIndexTree(model, rev string, dim int, prints map[string]string) {
	if len(prints) == 0 {
		return
	}
	byProj := map[string]map[string]string{}
	for id, fp := range prints {
		proj, br, path, ok := memory.SplitID(id)
		if !ok || proj == "" {
			continue
		}
		if byProj[proj] == nil {
			byProj[proj] = map[string]string{}
		}
		byProj[proj][br+":"+path] = fp
	}

	key := vecindex.ModelKeyFor(model, rev)
	file := key + ".json"
	now := time.Now().UTC()

	// 按项目稳定遍历，避免并发测试日志乱序。
	projs := make([]string, 0, len(byProj))
	for p := range byProj {
		projs = append(projs, p)
	}
	sort.Strings(projs)

	for _, proj := range projs {
		repo, err := s.projects.Get(proj)
		if err != nil {
			continue
		}
		slices := byProj[proj]
		snap := indexTreeSnapshot{
			Model:   model,
			Rev:     rev,
			Dim:     dim,
			Count:   len(slices),
			BuiltAt: now,
			Slices:  slices,
		}
		raw, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			continue
		}
		raw = append(raw, '\n')
		if err := repo.Write(indexTreeBranch, file, raw, gitstore.ModeContent); err != nil {
			s.logf("semantic: write index tree %s@%s:%s failed: %v", proj, indexTreeBranch, file, err)
			continue
		}
		// 异步投递给订阅了这棵树的上下文：索引结果已就绪。
		s.publishTreeChange(proj, indexTreeBranch, file, "", "",
			EventOrigin{Identity: "semantic:" + strings.TrimSpace(model), Endpoint: s.instanceID()})
	}
}
