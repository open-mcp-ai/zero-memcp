package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 命令流的应用与对象补齐。
//
// 这一层把「一条命令怎么落地」和「缺的 blob 怎么补」收在一处，
// 工作层（增量）与历史层（整树批量）共用——它们只是命令的两种粒度。
//
// 顺序是硬约束：**先 tree，后 blob**。
//
//	① 先应用命令：set/copy/rename 只登记 (mode, sha)，delete 立刻生效
//	② 缺对象的命令不落地，记进 Missing
//	③ 取回 blob、验真，再重放被卡住的那批命令
//
// 这个顺序消灭「离线节点复活已删内容」：墓碑先落地，那个 blob 从此
// 不在终局里，本地根本不会去请求它，机器上也不会有。

// opApplyResult 是应用一批命令的结果。
type opApplyResult struct {
	Applied  int      `json:"applied"`
	Deleted  int      `json:"deleted"`
	Missing  []string `json:"missing,omitempty"`
	Verified int      `json:"verified"`
	// Conflicted 是「本地也有未同步改动、内容不同」的路径。
	//
	// 这些路径**没有**被覆盖：保留本地版本，把对端版本记进冲突表等
	// 人工裁决。静默覆盖是最坏的解法——被吃掉的一方毫不知情。
	Conflicted []workConflict `json:"conflicted,omitempty"`
	// Rejected 是因故没能落地的命令（缺对象之外的原因）。
	Rejected []string `json:"rejected,omitempty"`
	// Kept 是权威整树里本地有未提交改动、因而拒绝被覆盖的路径。
	// 它们既不在 Applied 也不在 Deleted 里——必须能被看见。
	Kept []string `json:"kept,omitempty"`
	// DeletedPaths 是权威整树（Replace）里被墓碑的具体路径。
	//
	// 为什么需要它：Replace 的删除不是 ops 里的 opDelete，而是
	// applyTreeReplace 在运行时算出来的「本地有、批次没有」。
	// changedPathsOf 只看 ops，看不到这些路径，于是它们的索引
	// 永远不会被刷新——恢复出来的内容搜不到。
	DeletedPaths []string `json:"deleted_paths,omitempty"`
}

// opContext 是一次应用所需要的本地上下文。
//
// 把「本地工作层快照」抽出来只建一次：早先 importWorkDelta 与
// dropAlreadyInSync 各自建一份同样的 map，同一份事实读两遍。
type opContext struct {
	repo  *gitstore.Repo
	local map[string]gitstore.DirtyChange
	// clean 是已提交树里的路径表：判断「本地已有同样内容」时要用，
	// 否则一条远端命令只要本地脏层没记录就会被当成新改动重放。
	clean map[string]gitstore.Entry
}

func (s *Server) newOpContext(project, branch string) (*opContext, error) {
	// 落地命令流时自动建库：与 writeverb / writenote 保持一致。
	//
	// 踩过的坑（2026-11-22 备份实测抓到）：原实现用 projects.Get，
	// 结果一个全新的备份节点（配置里已声明项目、但 repos/ 下还没有 .git）
	// 收第一笔推流时直接报 500: project "X" has no repository yet。
	// 既然鉴权已在 handleOpPush 通过，就该在这里自动 init 裸库。
	repo, err := s.projects.Ensure(project)
	if err != nil {
		return nil, err
	}
	ctx := &opContext{repo: repo, local: map[string]gitstore.DirtyChange{}, clean: map[string]gitstore.Entry{}}

	changes, err := repo.DirtyChanges(branch)
	if err != nil {
		return nil, err
	}
	for _, ch := range changes {
		ctx.local[ch.Path] = ch
	}

	entries, err := repo.FlattenBranch(branch)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		ctx.clean[e.Path] = gitstore.Entry{Path: e.Path, Mode: e.Mode, Hash: e.SHA}
	}
	return ctx, nil
}

// currentSHA 报告某路径当前生效的 blob（含脏层），不存在则空。
func (ctx *opContext) currentSHA(path string) (string, bool) {
	if ch, ok := ctx.local[path]; ok {
		if ch.Deleted {
			return "", false
		}
		return ch.SHA, true
	}
	if e, ok := ctx.clean[path]; ok {
		return e.Hash, true
	}
	return "", false
}

// expand 把一条命令展开成可落地的原子操作。
//
// opTree 在这里展开成多条 set：这就是「历史层整树同步 = 批量命令流」
// 的落点。展开后与其它命令走完全相同的落地路径，没有第二套。
//
// Replace 为真时**展开不了**——它需要本地当前的路径表才能算出
// 「批次里没有 = 已删除」，那只能在 applyOps 里带上下文处理。
func expandOp(op treeOp) []treeOp {
	if op.Kind != opTree || op.Replace {
		return []treeOp{op}
	}
	out := make([]treeOp, 0, len(op.Batch))
	for _, e := range op.Batch {
		out = append(out, treeOp{
			Seq: op.Seq, Node: op.Node, Ts: op.Ts,
			Kind: opSet, Path: e.Path, Mode: e.Mode, SHA: e.SHA,
		})
	}
	return out
}

// applyTreeReplace 应用一条**权威整树**命令。
//
// 语义：批次里的路径按 set 落地；本地有、批次里没有、且本地没有
// 未提交改动的路径，一律墓碑——对端已经把它删了。
//
// 为什么必须这样（2026-11-22 部署前发现）：只展开成 set 的整树同步
// 只能加不能减。把一台落后八天的节点当成上游，会把八天里删掉的
// 内容全部复活；反过来，上游的删除也永远传不下来。
//
// 未提交改动一律保留：那是「正在想、还没说出口」的内容，不能被
// 一次远端整树覆盖掉。这类路径计入 Kept 并在日志点名。
func (s *Server) applyTreeReplace(project, branch, peerNode string, ctx *opContext, op treeOp, res *opApplyResult) error {
	inBatch := make(map[string]bool, len(op.Batch))
	for _, e := range op.Batch {
		inBatch[e.Path] = true
		one := treeOp{Seq: op.Seq, Node: op.Node, Ts: op.Ts, Kind: opSet, Path: e.Path, Mode: e.Mode, SHA: e.SHA}
		missing, err := s.applyOp(project, branch, peerNode, ctx, one, res)
		if err != nil {
			res.Rejected = append(res.Rejected, fmt.Sprintf("tree set %s: %v", e.Path, err))
			continue
		}
		if missing != "" {
			res.Missing = append(res.Missing, missing)
		}
	}

	// 本地有、批次里没有 → 对端已删除。
	var gone []string
	for path := range ctx.clean {
		if inBatch[path] {
			continue
		}
		if lch, ok := ctx.local[path]; ok && !lch.Deleted {
			// 本地有未提交改动：保留，不静默吃掉。
			res.Kept = append(res.Kept, path)
			continue
		}
		gone = append(gone, path)
	}
	sort.Strings(gone)
	for _, path := range gone {
		tomb := treeOp{Seq: op.Seq, Node: op.Node, Ts: op.Ts, Kind: opDelete, Path: path}
		if _, err := s.applyOp(project, branch, peerNode, ctx, tomb, res); err != nil {
			res.Rejected = append(res.Rejected, fmt.Sprintf("tree delete %s: %v", path, err))
			continue
		}
		// 记录被墓碑的路径，供 commitOpsLocal 刷新索引。
		// 没有它，Replace 删除的路径在索引里永远是旧的。
		res.DeletedPaths = append(res.DeletedPaths, path)
	}
	if len(res.Kept) > 0 {
		s.logf("op-sync: TREE REPLACE %s@%s kept %d locally-modified path(s): %s",
			project, branch, len(res.Kept), strings.Join(res.Kept, ", "))
	}

	// 整树快照落地后，把「这批内容对应的树」记成该对端的基准。
	//
	// 记账记在对端那一侧（peerBases），不动本端 oplog 的 baseTree：
	// 本端 oplog 的基准是「我这条命令流挂在哪棵树上」，是本地事实；
	// 对端手上有什么，是另一件事。混成一个字段会让接收方基准跑在
	// 发送方前面，于是发送方每轮都以为要重发整树（实测抓到的真 bug：
	// TestOpsSyncUncommittedAcrossNodes 追平后又 applied=1）。
	//
	// 两边各算各的同一棵树（内容寻址天然保证一致），增量才有共同的坐标原点。
	if op.Rebase && s.workSync != nil && len(op.Batch) > 0 {
		pathMap := make(map[string]gitstore.Entry, len(op.Batch))
		for _, e := range op.Batch {
			pathMap[e.Path] = gitstore.Entry{Path: e.Path, Mode: gitstore.Mode(e.Mode), Hash: e.SHA}
		}
		if tree, werr := ctx.repo.WriteTreeFromPaths(pathMap); werr == nil && tree != "" {
			s.workSync.noteBase(project, branch, peerNode, tree)
			// 本端此后也以这棵树为准：快照把工作层对齐到了它。
			if s.opLogs != nil {
				if l, lerr := s.opLogs.For(project, branch); lerr == nil {
					if rerr := l.Reset(tree); rerr != nil {
						s.logf("op-sync: reset oplog base to %s for %s@%s: %v", shortSHA(tree), project, branch, rerr)
					}
				}
			}
		}
	}
	return nil
}

// applyOp 应用一条已展开的命令。
//
// 返回值 missing 非空表示「这条等 blob」：不落地、不算错误，由调用方
// 取回对象后重放。
func (s *Server) applyOp(project, branch, peerNode string, ctx *opContext, op treeOp, res *opApplyResult) (missing string, err error) {
	switch op.Kind {
	case opDelete:
		// 删除与新增走同一条路径，只是没有 blob 要等。
		//
		// 但**绝不静默吃掉本地改动**：本地对这个路径有未提交的不同
		// 内容时，墓碑仍然落地（防复活不变量不可破），同时把被解链的
		// 本地 SHA 记进冲突表并在日志点名——用户必须知道有东西被删了。
		if lch, ok := ctx.local[op.Path]; ok && !lch.Deleted {
			s.recordWorkConflict(project, branch, op.Path, lch.SHA, "", peerNode, ConflictDelete)
			s.logf("op-sync: DELETE WINS %s@%s:%s (local uncommitted %s was unlinked)",
				project, branch, op.Path, shortSHA(lch.SHA))
		}
		if err := ctx.repo.ApplyChange(branch, op.Path, gitstore.ModeContent, "", true); err != nil {
			return "", fmt.Errorf("apply delete %s: %w", op.Path, err)
		}
		delete(ctx.local, op.Path)
		delete(ctx.clean, op.Path)
		res.Deleted++
		return "", nil

	case opSet:
		if op.SHA == "" || !ctx.repo.HasObject(op.SHA) {
			// 正文还没到：记下来，第二趟再取。
			// 注意这里**不写工作层**——写一条指向空气的条目，读取时才会炸，
			// 而且爆在完全无关的地方。
			return op.SHA, nil
		}
		// 相同内容不算冲突：那是同一份改动的重复投递，幂等即可。
		if lch, ok := ctx.local[op.Path]; ok && !lch.Deleted && lch.SHA != op.SHA {
			res.Conflicted = append(res.Conflicted, workConflict{
				Path: op.Path, LocalSHA: lch.SHA, RemoteSHA: op.SHA, Peer: peerNode,
			})
			s.recordWorkConflict(project, branch, op.Path, lch.SHA, op.SHA, peerNode, ConflictSet)
			s.logf("op-sync: CONFLICT %s@%s:%s (local %s vs peer %s from %s) — kept local copy",
				project, branch, op.Path, shortSHA(lch.SHA), shortSHA(op.SHA), peerNode)
			return "", nil
		}
		// 已与本地一致（含已提交层）：跳过，不重写、不重刷、不重广播。
		if cur, ok := ctx.currentSHA(op.Path); ok && cur == op.SHA {
			if _, dirty := ctx.local[op.Path]; !dirty {
				return "", nil
			}
		}
		if err := ctx.repo.ApplyChange(branch, op.Path, gitstore.Mode(op.Mode), op.SHA, false); err != nil {
			return "", fmt.Errorf("apply set %s: %w", op.Path, err)
		}
		ctx.local[op.Path] = gitstore.DirtyChange{Path: op.Path, Mode: gitstore.Mode(op.Mode), SHA: op.SHA}
		s.clearWorkConflict(project, branch, op.Path)
		res.Applied++
		return "", nil

	case opRename, opCopy:
		// 结构操作：源路径当前指向什么，目标就指向什么。
		//
		// 内容用哈希引用，不复制字节——blob 在对象库里只有一份。
		src, ok := ctx.currentSHA(op.From)
		if !ok || src == "" {
			return "", fmt.Errorf("%s %s: source path does not exist locally", op.Kind, op.From)
		}
		if !ctx.repo.HasObject(src) {
			return src, nil
		}
		if lch, ok := ctx.local[op.Path]; ok && !lch.Deleted && lch.SHA != src {
			res.Conflicted = append(res.Conflicted, workConflict{
				Path: op.Path, LocalSHA: lch.SHA, RemoteSHA: src, Peer: peerNode,
			})
			s.recordWorkConflict(project, branch, op.Path, lch.SHA, src, peerNode, ConflictSet)
			return "", nil
		}
		mode := op.Mode
		if mode == "" {
			mode = string(gitstore.ModeContent)
		}
		if err := ctx.repo.ApplyChange(branch, op.Path, gitstore.Mode(mode), src, false); err != nil {
			return "", fmt.Errorf("apply %s %s: %w", op.Kind, op.Path, err)
		}
		ctx.local[op.Path] = gitstore.DirtyChange{Path: op.Path, Mode: gitstore.Mode(mode), SHA: src}
		res.Applied++
		if op.Kind == opRename {
			// rename 的源删除与目标新增在同一条命令里，对端不会看到中间态。
			if err := ctx.repo.ApplyChange(branch, op.From, gitstore.ModeContent, "", true); err != nil {
				return "", fmt.Errorf("apply rename source %s: %w", op.From, err)
			}
			delete(ctx.local, op.From)
			delete(ctx.clean, op.From)
			res.Deleted++
		}
		return "", nil
	}
	return "", fmt.Errorf("unknown op kind %q", op.Kind)
}

// applyOps 应用一批命令，返回结果与「还缺哪些对象」。
func (s *Server) applyOps(project, branch, peerNode string, ops []treeOp) (opApplyResult, error) {
	var res opApplyResult
	ctx, err := s.newOpContext(project, branch)
	if err != nil {
		return res, err
	}
	seenMissing := map[string]bool{}
	for _, raw := range ops {
		if raw.Kind == opTree && raw.Replace {
			if terr := s.applyTreeReplace(project, branch, peerNode, ctx, raw, &res); terr != nil {
				res.Rejected = append(res.Rejected, fmt.Sprintf("tree replace: %v", terr))
			}
			continue
		}
		for _, op := range expandOp(raw) {
			missing, aerr := s.applyOp(project, branch, peerNode, ctx, op, &res)
			if aerr != nil {
				res.Rejected = append(res.Rejected, fmt.Sprintf("seq %d %s %s: %v", op.Seq, op.Kind, op.Path, aerr))
				continue
			}
			if missing != "" && !seenMissing[missing] {
				seenMissing[missing] = true
				res.Missing = append(res.Missing, missing)
			}
		}
	}
	sort.Strings(res.Missing)
	return res, nil
}

// changedPathsOf 取出一批命令实际触碰的路径，供索引刷新与广播使用。
func changedPathsOf(ops []treeOp) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range ops {
		// opTree + Replace 的整树命令不展开成 opSet，但 Batch 里每条
		// 条目都是真实变更的路径。忽略它们会导致「恢复出来的内容
		// 在索引里看不见」——恢复节点的 docs_indexed 恒为 0。
		if raw.Kind == opTree && raw.Replace {
			for _, e := range raw.Batch {
				if e.Path != "" && !seen[e.Path] {
					seen[e.Path] = true
					out = append(out, e.Path)
				}
			}
			continue
		}
		for _, op := range expandOp(raw) {
			for _, p := range []string{op.Path, op.From} {
				if p != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// commitOpsLocal 在一批命令落地之后，刷新索引、广播变更、登记水位。
//
// 这是**唯一**的落盘后置动作：push 接收端与 pull 落地端都走这里。
// 早先两处各写了一遍「refreshPath + publishTreeChange」，那种重复
// 会让「磁盘有数据但搜不到」这种故障在一侧复发而另一侧正常。
func (s *Server) commitOpsLocal(project, branch, peerNode string, ops []treeOp, res opApplyResult) {
	for _, cf := range res.Conflicted {
		kind := cf.Kind
		if kind == "" {
			kind = ConflictSet
		}
		s.recordWorkConflict(project, branch, cf.Path, cf.LocalSHA, cf.RemoteSHA, cf.Peer, kind)
	}
	paths := changedPathsOf(ops)
	// Replace 整树算出来的墓碑不在 ops 里，但也必须刷新索引。
	for _, p := range res.DeletedPaths {
		if !containsStr(paths, p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := s.refreshPath(project, branch, p, true); err != nil {
			s.logf("op-sync: refresh %s@%s:%s failed: %v", project, branch, p, err)
			// 索引刷新失败不是致命错误，但必须出声——否则表现为
			// 「数据在、搜不到」，最难查的一类。
			continue
		}
		s.publishTreeChange(project, branch, p, "", "",
			EventOrigin{Identity: "peer:" + peerNode, Endpoint: s.instanceID()})
	}
	if len(paths) > 0 {
		s.logf("op-sync: %s@%s from %s: applied %d, deleted %d, %d path(s) refreshed, %d conflicted, %d missing",
			project, branch, peerNode, res.Applied, res.Deleted, len(paths), len(res.Conflicted), len(res.Missing))
	}
}
