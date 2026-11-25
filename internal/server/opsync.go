package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 命令流同步引擎：推与拉都走同一份日志。
//
// 与旧实现（syncwork.go 的脏层快照搬运）的关系：
//
//	旧：export 整张脏层表 → 对端比对 → 缺啥补啥 → 靠 dropAlreadyInSync 去重
//	新：export 位点之后的命令 → 对端按位点应用 → 缺 blob 就记 Missing
//	    → 取回验真后重放 → 位点连续即无丢失
//
// 推与拉共用的不只是传输代码，还有**应用与落地**（applyOps /
// commitOpsLocal）：两个方向收到的命令走完全相同的一条路。

// exportOps 取出对端还没见过的命令。
//
// batch.Node 是本节点的名字：它是这份日志的**真实属主**，对端必须
// 用它来记水位。不能用本地配置里的对端别名——同一台机器在不同
// 配置里可能叫不同名字，而日志属主只有一个。

// diffTreesToOps 比对两棵树，把差异编码成一组普通的 set/delete 命令。
//
// 为什么必须有它（2026-11-24 根治整树重发）：
// commit 发生后，本端命令流归零、基准前进。对端手上还停在
// 上一次同步后的内容树上——两边基准对不上。
// 早先遇到基准对不上就无脑发「整树快照（Replace=true）」，
// 导致即使只改了 1 篇文档，也要每轮把 18,446 个条目全部序列化、
// 网络传输、对端逐个比对。不仅浪费带宽和 CPU，还在日志里造成
// 「永不收敛」的假象。
//
// 现在：先看本地对象库里认不认得对端手上的 baseTree。
// 认得就直接 Diff 出增量命令（几十字节），声明 BaseTree = baseTree。
// 接收端校验通过，只应用这几条增量。认不得才退回整树快照。
func (s *Server) diffTreesToOps(project, branch, fromTree, toTree string) ([]treeOp, error) {
	repo, err := s.projects.Get(project)
	if err != nil {
		return nil, err
	}
	changes, err := repo.Diff(fromTree, toTree)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	node := s.instanceID()
	ops := make([]treeOp, 0, len(changes))
	for i, c := range changes {
		seq := int64(i + 1)
		switch c.Kind {
		case "added", "modified":
			ops = append(ops, treeOp{
				Seq:  seq,
				Node: node,
				Ts:   now,
				Kind: opSet,
				Path: c.Path,
				Mode: string(c.Mode),
				SHA:  c.Hash,
			})
		case "deleted":
			ops = append(ops, treeOp{
				Seq:  seq,
				Node: node,
				Ts:   now,
				Kind: opDelete,
				Path: c.Path,
			})
		}
	}
	return ops, nil
}
func (s *Server) exportOps(project, branch string, after int64) (opBatch, error) {
	return s.exportOpsFrom(project, branch, after, "")
}

// exportOpsFrom 在 exportOps 之上支持「以某个基准树为准」。
//
// 命令流的位点只在**同一段流**里可比：基准树一变，所有位点都要重新
// 解释。所以基准先于位点——对端报的基准与本端不同，位点就毫无意义。
//
// 对不上时不发明细，而是告诉对端「我现在的基准是这棵树，它对应的提交
// 是这一个」。对端去浅拷贝那个提交，然后带着新基准再来。
func (s *Server) exportOpsFrom(project, branch string, after int64, baseTree string) (opBatch, error) {
	return s.exportOpsFromPeer(project, branch, after, baseTree, "")
}

// exportOpsFromPeer 与 exportOpsFrom 相同，但知道对端是谁。
//
// 知道对端之后多一种「已经对齐」的判断：对端手上的基准可能不是本端
// oplog 的原点（4b825dc6），而是我们上次发给它的那棵内容树。整树快照
// 落地后对端会把自己的基准重设到那棵树，两边算出的 SHA 完全相同——
// 内容寻址保证的。认不出这一种，就会每轮重发整棵树。
func (s *Server) exportOpsFromPeer(project, branch string, after int64, baseTree, peerNode string) (opBatch, error) {
	batch := opBatch{Project: project, Branch: branch, Node: s.instanceID(), From: after}
	if s.opLogs == nil {
		return batch, fmt.Errorf("oplog is not initialised")
	}
	l, err := s.opLogs.For(project, branch)
	if err != nil {
		return batch, err
	}
	cur := l.BaseTree()

	// 判据只用**内容树**，不用「我记得发过什么」。
	//
	// 早先这里有一条捷径：对端报的基准等于上次发给它的内容树，就当成
	// 「已对齐」发位点增量。它错在忽略了一件事——本端一提交，命令流就
	// 归零，那批增量命令已经被折进新基准里，位点增量是空的。于是
	// 「对端明明落后，我们却回了 0 条命令」，备份静默停摆。
	// 内容树是确定的：相等就是真的相等，不等就必须算出差多少。
	if baseTree == "" {
		// 调用方只是要增量命令（exportOps），不关心基准。
		batch.BaseTree = cur
		batch.BaseCommit = s.commitForTree(project, branch, cur)
		return s.exportOpsTail(batch, l, after, cur)
	}

	if baseTree == cur {
		// 对端就在本端命令流的原点上：位点增量就是全部差异。
		batch.BaseTree = cur
		batch.BaseCommit = s.commitForTree(project, branch, cur)
		return s.exportOpsTail(batch, l, after, cur)
	}

	// 基准不同：把两边都算成内容树，用内容判断差多少。
	entries, ferr := s.flattenTreeToOps(project, branch)
	if ferr != nil {
		return batch, ferr
	}
	// 本端一棵空树、对端却有内容：这是唯一会把对端清空的形状，必须拒绝。
	if len(entries) == 0 && baseTree != gitstore.EmptyTreeSHA {
		return batch, fmt.Errorf(
			"refusing to send an empty-tree snapshot for %s@%s: peer base %s is not empty, "+
				"this would erase everything on the peer", project, branch, shortSHA(baseTree))
	}
	var curContentTree string
	if repo, rerr := s.projects.Get(project); rerr == nil && len(entries) > 0 {
		pathMap := make(map[string]gitstore.Entry, len(entries))
		for _, e := range entries {
			pathMap[e.Path] = gitstore.Entry{Path: e.Path, Mode: gitstore.Mode(e.Mode), Hash: e.SHA}
		}
		if tree, werr := repo.WriteTreeFromPaths(pathMap); werr == nil {
			curContentTree = tree
		}
	}

	// 对端手上的内容已经和本端一模一样：一个字节都不发。
	//
	// 这是提交之后最常见的一轮——对端上次收到的就是本端当前的内容，
	// 只是本端基准又前进了一次。认不出这种情况就会每轮重发整棵树。
	if curContentTree != "" && curContentTree == baseTree {
		if s.workSync != nil && peerNode != "" {
			s.workSync.noteSentTree(project, branch, peerNode, curContentTree)
		}
		batch.To = l.Last()
		return batch, nil
	}

	// 首选：树到树差分，只发变化的路径。
	//
	// 对端报的基准是一棵真实的树（不是空树），且本地对象库里存着它，
	// 就能直接算出差异。18,446 篇里改 1 篇就只发 1 条命令。
	if baseTree != gitstore.EmptyTreeSHA && curContentTree != "" {
		if diffOps, derr := s.diffTreesToOps(project, branch, baseTree, curContentTree); derr == nil {
			batch.BaseTree = baseTree
			batch.BaseCommit = s.commitForTree(project, branch, baseTree)
			batch.Rebase = true
			batch.Ops = diffOps
			batch.To = l.Last()
			if s.workSync != nil && peerNode != "" {
				s.workSync.noteSentTree(project, branch, peerNode, curContentTree)
			}
			s.logf("op-sync: %s@%s: peer base %s -> content %s: %d delta op(s) (tree diff, no full snapshot)",
				project, branch, shortSHA(baseTree), shortSHA(curContentTree), len(diffOps))
			return batch, nil
		}
	}

	// 回退：对端是空树，或本地没有对端那棵树（认不出差异只能整树给）。
	batch.BaseTree = cur
	batch.BaseCommit = s.commitForTree(project, branch, cur)
	batch.Rebase = true
	if s.workSync != nil && peerNode != "" && curContentTree != "" {
		s.workSync.noteSentTree(project, branch, peerNode, curContentTree)
	}
	batch.Ops = []treeOp{{
		Kind:    opTree,
		Replace: true,
		Rebase:  true,
		Batch:   entries,
		Ts:      time.Now().Unix(),
	}}
	batch.To = l.Last()
	s.logf("op-sync: %s@%s: peer base %s != local base %s; sending full tree snapshot (%d entries)",
		project, branch, shortSHA(baseTree), shortSHA(cur), len(entries))
	return batch, nil
}

// exportOpsTail 收尾两条「对端就在本端原点上」的路径：按位点发增量。
//
// 位点只在同一段流里可比：提交会让流归零，位点从 1 重来。对端报的位点
// 超过本端流长，说明它记的是上一段流的位点——此时按 0 处理（从新流的
// 开头发），而不是发空批次把对端晾在原地。
func (s *Server) exportOpsTail(batch opBatch, l *opLog, after int64, cur string) (opBatch, error) {
	// 读侧播种：对端从位点 0 开始拉、而本地日志尚为空且基准是空树时，
	// 先把当前整棵树作为第一条批量命令播进日志。
	//
	// 注意它与整树快照互斥：快照走 Rebase 分支，播种走普通日志。
	// 有了 Rebase 区分，落地端知道这条只是普通命令，不会重设基准。
	if after == 0 && l.Last() == 0 && cur == gitstore.EmptyTreeSHA {
		if serr := l.seedIfEmpty(); serr != nil {
			s.logf("oplog: seed initial tree for %s@%s: %v", batch.Project, batch.Branch, serr)
		}
	}
	if after > l.Last() {
		after = 0
	}
	batch.Ops = l.Since(after)
	batch.To = l.Last()
	return batch, nil
}

// commitForTree 返回当前分支上「树等于 tree」的那个提交（没有则空）。
//
// 用途只有一个：告诉对端该浅拷贝哪个提交。基准是空树时返回空——
// 分支还没有任何提交，对端不需要拉任何东西。
func (s *Server) commitForTree(project, branch, tree string) string {
	if tree == "" || tree == gitstore.EmptyTreeSHA {
		return ""
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		return ""
	}
	head, err := repo.Resolve(branch)
	if err != nil || head == "" {
		return ""
	}
	headTree, err := repo.TreeOfCommit(head)
	if err != nil || headTree != tree {
		// 头部不是这个树：说明基准落后于头部，那就不给提交，
		// 让对端走「整树重发」这条老路。宁可贵一点，不要给错的提交。
		return ""
	}
	return head
}

// ingestOps 把对端推来的一批命令接进本地：应用 → 补 blob → 重放。
//
// 这是 server 端 handleOpPush 与本端 pull 落地共用的入口。
func (s *Server) ingestOps(project, branch string, batch opBatch) (opApplyResult, error) {
	// 基准树校验：整树快照会重设基准（Replace 语义），允许；
	// 增量命令只在基准相同时才成立——不同就是位点错位，拼出来的是
	// 一棵既不是这里也不是那里的树，必须出声拒绝。
	if batch.BaseTree != "" && s.opLogs != nil && !isSnapshotBatch(batch) {
		if l, lerr := s.opLogs.For(project, branch); lerr == nil {
			if cur := l.BaseTree(); cur != batch.BaseTree {
				var zero opApplyResult
				return zero, fmt.Errorf(
					"op-sync %s@%s: base tree mismatch (local %s, peer %s); ask the peer for a full snapshot",
					project, branch, shortSHA(cur), shortSHA(batch.BaseTree))
			}
		}
	}

	res, err := s.applyOps(project, branch, batch.Node, batch.Ops)
	if err != nil {
		return res, err
	}
	s.commitOpsLocal(project, branch, batch.Node, batch.Ops, res)

	// 批次级重设基准：整树快照与树到树差分都带这个标记。
	//
	// 它们的差异是**相对对端手上那棵树**算出来的，落地后本端内容就跑到
	// 了发送端的位置上，旧位点、旧基准全部作废。必须把命令流基准重设到
	// 「落地后的内容树」，否则下一轮发送端按新树算差分，本端还停在旧树上，
	// 校验直接拒收：base tree mismatch。
	//
	// 放在批次级而不是单条 op 上，是因为差分批次里没有任何一条 op 是
	// 「整树命令」——它只是一串普通的 set/delete，只有批次本身知道
	// 「这批合起来等于一棵新树」。
	if batch.Rebase {
		s.rebaseLocalToContent(project, branch, batch.Node)
	}
	s.learnPeerIdentity(batch.Node, batch.Node)
	// 只有在命令真正被处理之后才推进对端水位：
	// 「收到了」不等于「落地了」，先推进水位会让丢命令变成永久丢失。
	if batch.To > 0 {
		s.notePeerSeq(project, branch, batch.Node, batch.To)
	}
	// 记下「这个水位属于哪段流」：对端流归零后位点会重来，
	// 不绑基准就会出现「数字对得上、含义完全不同」的静默停摆。
	if s.workSync != nil && s.opLogs != nil {
		if l, lerr := s.opLogs.For(project, branch); lerr == nil {
			s.workSync.noteBase(project, branch, batch.Node, l.BaseTree())
		}
	}
	return res, nil
}


// rebaseLocalToContent 把本端命令流的基准重设到「当前工作层的完整内容树」。
//
// 只在收到带 Rebase 标记的批次后调用：那批命令已经把本端内容对齐到了
// 发送端，于是本端的坐标原点也该跟着走。
func (s *Server) rebaseLocalToContent(project, branch, peerNode string) {
	repo, err := s.projects.Get(project)
	if err != nil {
		return
	}
	flat, ferr := repo.FlattenBranch(branch)
	if ferr != nil || len(flat) == 0 {
		return
	}
	pathMap := make(map[string]gitstore.Entry, len(flat))
	for _, e := range flat {
		pathMap[e.Path] = gitstore.Entry{Path: e.Path, Mode: e.Mode, Hash: e.SHA}
	}
	tree, werr := repo.WriteTreeFromPaths(pathMap)
	if werr != nil || tree == "" {
		return
	}
	if s.workSync != nil && peerNode != "" {
		s.workSync.noteBase(project, branch, peerNode, tree)
	}
	if s.opLogs != nil {
		if l, lerr := s.opLogs.For(project, branch); lerr == nil {
			if rerr := l.Reset(tree); rerr != nil {
				s.logf("op-sync: reset oplog base to %s for %s@%s: %v", shortSHA(tree), project, branch, rerr)
			}
		}
	}
}
func (s *Server) notePeerSeq(project, branch, peerNode string, seq int64) {
	noteWatermark(&s.workSync.in, workKey(project, branch, peerNode), seq)
}

// peerSeq 读出我对某个对端节点的同步游标。
func (s *Server) peerSeq(project, branch, peerNode string) int64 {
	_, in := s.workSync.cursors(project, branch, peerNode)
	return in
}

// peerOutSeq 是「对端已确认收到我本地日志的哪一位」。
//
// 推与拉的水位绝不能共用（2026-11-22 狗粮实测抓到）：
// 镜像从上游拉到 seq 80 后，若推方向也读 in=80，就会拿它去截
// 自己只有 1 条的本地日志，永远导出空批次；status 却仍报同步成功。
func (s *Server) peerOutSeq(project, branch, peerNode string) int64 {
	out, _ := s.workSync.cursors(project, branch, peerNode)
	return out
}

// learnPeerIdentity 记住「配置里的对端别名 ↔ 对端自报的日志属主名」。
//
// 为什么必须有这张表（测试抓到的真 bug）：
//
//	拉取开始时我只知道配置里的 peerName；而对端批次里带的是它自己的
//	instanceID。ingestOps 按日志属主记水位，pull 却按 peerName 查——
//
// 两个键不同，于是每轮都从头拉、每轮都重复应用同一批命令。
//
// 两个方向统一在这里解析：见过一次属主名之后，后续都用它。
func (s *Server) learnPeerIdentity(peerName, nodeID string) {
	if peerName == "" || nodeID == "" || peerName == nodeID {
		return
	}
	s.peerAliases.Store(peerName, nodeID)
}

// resolvePeer 把配置别名解析成日志属主名（若已学到）。
func (s *Server) resolvePeer(peerName string) string {
	if v, ok := s.peerAliases.Load(peerName); ok {
		return v.(string)
	}
	return peerName
}

// ── HTTP 端点 ────────────────────────────────────────────────

// handleOpPull 对端来要命令：给它位点之后的部分。
func (s *Server) handleOpPull(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		Project string `json:"project"`
		Branch  string `json:"branch"`
		Node    string `json:"node"`
		After   int64  `json:"after"`
		// BaseTree 是对端当前的基准树。
		// 双方基准相同才发增量命令；不同就返回 NeedRebase 让对端先浅拉。
		BaseTree string `json:"base_tree,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad op-pull request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.allowed(c, req.Project, req.Branch, configBitRead) {
		http.Error(w, s.denyReason(c, req.Project, req.Branch, configBitRead), http.StatusForbidden)
		return
	}
	batch, err := s.exportOpsFromPeer(req.Project, req.Branch, req.After, req.BaseTree, req.Node)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 上游视角：谁在什么时候把我拉到了哪一位。
	//
	// 这条记录让「在上游问记忆体情况」能回答：有哪些下游、各自追到哪了、
	// 有没有落下的。没有它，上游只知道自己在写，不知道谁跟上了。
	s.recordSync(syncRecord{
		Dir: "served", Peer: req.Node, Node: req.Node,
		Project: req.Project, Branch: req.Branch,
		From: req.After, To: batch.To, PeerHead: int(batch.From),
		Applied: len(batch.Ops),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(batch)
}

// handleOpPush 对端推来一批命令。
func (s *Server) handleOpPush(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var batch opBatch
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&batch); err != nil {
		http.Error(w, "bad op-push request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.allowed(c, batch.Project, batch.Branch, configBitWrite) {
		http.Error(w, s.denyReason(c, batch.Project, batch.Branch, configBitWrite), http.StatusForbidden)
		return
	}
	res, err := s.ingestOps(batch.Project, batch.Branch, batch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 被推方视角：某个下游把它的命令推给了我。
	s.recordSync(syncRecord{
		Dir: "received", Peer: batch.Node, Node: batch.Node,
		Project: batch.Project, Branch: batch.Branch,
		From: batch.From, To: batch.To,
		Applied: res.Applied, Deleted: res.Deleted, Kept: len(res.Kept),
		Conflicted: len(res.Conflicted), Missing: len(res.Missing),
		Rejected: len(res.Rejected),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// ── 客户端：向对端要 / 推 ────────────────────────────────────

func postOpPull(client *http.Client, baseURL, token, project, branch, node string, after int64) (opBatch, error) {
	return postOpPullFrom(client, baseURL, token, project, branch, node, after, "")
}

// postOpPullFrom 带基准提交的拉取。
//
// baseCommit 非空时告诉对端「我已经有这棵树了，只要它之后的命令」。
// 对端认不出这个提交时会自动退化为整树，不会报错。
func postOpPullFrom(client *http.Client, baseURL, token, project, branch, node string, after int64, baseTree string) (opBatch, error) {
	var batch opBatch
	payload := map[string]any{"project": project, "branch": branch, "node": node, "after": after}
	if baseTree != "" {
		payload["base_tree"] = baseTree
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/sync/ops/pull", bytes.NewReader(body))
	if err != nil {
		return batch, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return batch, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return batch, err
	}
	if resp.StatusCode != http.StatusOK {
		return batch, fmt.Errorf("op-pull %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, &batch); err != nil {
		return batch, err
	}
	return batch, nil
}

func postOpPush(client *http.Client, baseURL, token string, batch opBatch) (opApplyResult, error) {
	var res opApplyResult
	body, err := json.Marshal(batch)
	if err != nil {
		return res, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/sync/ops/push", bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return res, err
	}
	if resp.StatusCode != http.StatusOK {
		var zero opApplyResult
				return zero, fmt.Errorf("op-push %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return res, err
	}
	return res, nil
}

// pushOpsToPeer 把本地位点之后的命令推给对端，并按需补 blob。
//
// 与 pullOpsFromPeer 是严格对偶：两者都是「先命令、后对象、再重放」。
func (s *Server) pushOpsToPeer(client *http.Client, baseURL, token, peerName, project, branch string) (opApplyResult, error) {
	var zero opApplyResult
	if s.opLogs == nil {
		return zero, fmt.Errorf("oplog is not initialised")
	}
	l, err := s.opLogs.For(project, branch)
	if err != nil {
		return zero, err
	}
	// 推的水位：我作为日志属主，对端（peerName）确认到哪一位。
	// 推方向由本端记，因为对端不会主动告诉我它收到哪了。
	peerKey := s.resolvePeer(peerName)
	curBase := l.BaseTree()
	// 对端手上是哪棵树——用 peerTrees，不是 bases（见 syncwork.go 的说明）。
	peerBase := ""
	if s.workSync != nil {
		peerBase = s.workSync.peerTreeOf(project, branch, peerKey)
	}
	// 要不要发权威整树快照，判据是「对端记得的基准与本端不同」。
	//
	// 判据不能是「对端水位为 0」：命令流提交即归零，归零后的流 last 也是 0，
	// 快照发完之后水位仍然停在 0。用位点判会把「已经对齐」误判成「还没发过」，
	// 于是每轮同步都重发整棵树——实测 rock64 每 3 分钟收一次 13,627 个路径，
	// 永远收敛不了。基准才是这段流的坐标原点，位点只是流内的偏移。
	// 对端基准：本端记录的 peerBase；没有则用上次快照树。
	effectivePeerBase := peerBase
	if effectivePeerBase == "" && s.workSync != nil {
		effectivePeerBase = s.workSync.sentTreeOf(project, branch, peerKey)
	}
	// 对端从未同步过：它的基准就是官方空树（一个确定的值，不是「未知」）。
	// 这样对端如果是空的，而本端已经有提交（curBase != EmptyTreeSHA），
	// 就会自然触发整树快照把基准树里的内容送过去。
	if effectivePeerBase == "" {
		effectivePeerBase = gitstore.EmptyTreeSHA
	}
	// 基准变了，水位就没有意义：旧位点在新流里不可比。
	// 最坏情况是恰好「对得上」（旧流 seq 1 与新流 seq 1 数字相同），
	// Since(1) 返回空，于是这条备份再也不会前进，而且一声不响。
	// 水位是否作废，只看**本端流换没换原点**，与对端手上是哪棵树无关。
	// 对端收下快照不等于我的流换了原点；混在一起会把水位无谓地抹掉。
	if s.workSync != nil {
		if sb := s.workSync.baseOf(project, branch, peerKey); sb != "" && sb != curBase {
			s.workSync.dropWatermarks(project, branch, peerKey)
		}
	}
	after := s.peerOutSeq(project, branch, peerKey)
	if after > l.Last() {
		after = 0
	}
	// exportOpsFromPeer 自己判断「对齐还是发快照」：
	// 认得出对端手上的树是本端流的某个前缀（含上次发的内容树）就发增量。
	//
	// 包成闭包是为了**重算一次**：被对端以基准不符拒绝之后，本端对
	// 「对端手上是哪棵树」的记忆就已经被证伪，必须抹掉重导（见下）。
	exportBatch := func() (opBatch, error) {
		peerBase := ""
		if s.workSync != nil {
			peerBase = s.workSync.peerTreeOf(project, branch, peerKey)
		}
		if peerBase == "" && s.workSync != nil {
			peerBase = s.workSync.sentTreeOf(project, branch, peerKey)
		}
		if peerBase == "" {
			peerBase = gitstore.EmptyTreeSHA
		}
		return s.exportOpsFromPeer(project, branch, after, peerBase, peerKey)
	}
	batch, err := exportBatch()
	if err != nil {
		return zero, err
	}
	if len(batch.Ops) == 0 {
		return zero, nil
	}
	res, err := postOpPush(client, baseURL, token, batch)
	if err != nil && isBaseTreeMismatch(err) {
		// 对端说「我不认识你那个基准」。它对，本端那个记忆确实过期了——
		// 对端离线期间本端折过 commit，内容树已经走到别处。
		//
		// 早先这里直接把错误抛出去，下一次、再下一次还是同一批、同一个
		// 位置，而错误信息在叫人手工去要快照。没有人会去，备份就永久停住。
		s.logf("op-sync: %s@%s: peer %s rejected our base (stale memory); re-sending as a full snapshot",
			project, branch, peerKey)
		if s.workSync != nil {
			s.workSync.forgetPeerTree(project, branch, peerKey)
			s.workSync.forgetSentTree(project, branch, peerKey)
		}
		var rerr error
		batch, rerr = exportBatch()
		if rerr != nil {
			return zero, rerr
		}
		if len(batch.Ops) == 0 {
			return zero, nil
		}
		res, err = postOpPush(client, baseURL, token, batch)
	}
	if err != nil {
		return res, err
	}
	if len(res.Missing) > 0 {
		repo, rerr := s.projects.Get(project)
		if rerr != nil {
			return res, rerr
		}
		seen := map[string]bool{}
		for _, sha := range res.Missing {
			if seen[sha] {
				continue
			}
			seen[sha] = true
			body, berr := repo.BlobBody(sha)
			if berr != nil {
				var zero opApplyResult
				return zero, fmt.Errorf("read local blob %s: %w", sha, berr)
			}
			if perr := postWorkBlob(client, baseURL, token, project, branch, sha, body); perr != nil {
				var zero opApplyResult
				return zero, fmt.Errorf("push blob %s: %w", sha, perr)
			}
		}
		// 第二趟：对象已到对面并验真，现在重放同一批命令。
		res, err = postOpPush(client, baseURL, token, batch)
		if err != nil {
			return res, err
		}
		if len(res.Missing) > 0 {
			var zero opApplyResult
				return zero, fmt.Errorf("peer still reports %d missing blob(s) after upload", len(res.Missing))
		}
	}
	// 对端拒收的命令必须出声。
	//
	// 记录里只写 applied 是不够的：拒收的命令既不会被重传（不在 Missing 里），
	// 也不会保留本地（不是冲突），它们就是没到。不点名的话发送方看到
	// "applied N" 会以为同步成功——备份静默地只到一半。
	if len(res.Rejected) > 0 {
		s.logf("op-sync: peer %s REJECTED %d op(s) for %s@%s — these did NOT land: %s",
			peerName, len(res.Rejected), project, branch, func() string {
			if len(res.Rejected) <= 3 {
				return strings.Join(res.Rejected, "; ")
			}
			return strings.Join(res.Rejected[:3], "; ") + fmt.Sprintf(" ... (+%d more)", len(res.Rejected)-3)
		}())
	}
	// 只有对端确认之后才推进本地水位，并记下这个水位属于哪段流。
	s.workSync.noteOut(project, branch, peerName, l.Last())
	// 记给对端的基准：快照落地后对端手上的树是内容树，不是本端原点。
	// 记错了下一轮就会按原点声明增量，对端按内容树校验，永远对不上。
	// 对端手上的内容推进到哪了：
	//   整树快照 → 我们算出的那棵内容树
	//   差分批次 → 也是那棵内容树（对端收下全部增量后内容就等于它）
	//   普通增量 → 同上，内容跟着前进
	// 三种情况其实一致：对端收下这批之后，内容就是本端当前的内容。
	//
	// 认错了的后果是每轮都判定「对端落后」→ 每轮重算并重发同一批差分，
	// 备份永不收敛（这正是本轮要根治的形状）。
	newPeerTree := batch.BaseTree
	if st := s.workSync.sentTreeOf(project, branch, peerKey); st != "" {
		newPeerTree = st
	}
	s.workSync.notePeerTree(project, branch, peerKey, newPeerTree)
	s.workSync.noteBase(project, branch, peerKey, curBase)
	s.recordSync(syncRecord{
		Dir: "push", Peer: peerName, Node: s.resolvePeer(peerName),
		Project: project, Branch: branch,
		From: after, To: l.Last(), PeerHead: len(batch.Ops),
		Applied: res.Applied, Deleted: res.Deleted, Kept: len(res.Kept),
		Conflicted: len(res.Conflicted), Missing: len(res.Missing),
		Rejected: len(res.Rejected),
	})
	// 每轮推进后立即落盘：进程被 kill 不会走 Close，
	// 水位留在内存就等于下次重启全量重拉。
	if serr := s.saveOpSyncState(); serr != nil {
		s.logf("opsync: save watermark after push: %v", serr)
	}
	return res, nil
}

// firstBlockedSeq 返回「缺正文的命令」里最小的那个 seq。
//
// 水位要钉在它之前：水位是「到这儿为止都落地了」的声明，而这条没落地。
func firstBlockedSeq(ops []treeOp, missing []string) int64 {
	want := make(map[string]bool, len(missing))
	for _, sha := range missing {
		want[sha] = true
	}
	var first int64
	for _, op := range ops {
		if op.SHA == "" || !want[op.SHA] {
			continue
		}
		if first == 0 || op.Seq < first {
			first = op.Seq
		}
	}
	return first
}

// seqOfSHA 找出某份正文对应的命令位点（留证要能点名「卡在哪一条」）。
func seqOfSHA(ops []treeOp, sha string) int64 {
	for _, op := range ops {
		if op.SHA == sha && op.Seq != 0 {
			return op.Seq
		}
	}
	return 0
}

// pathOfSHA 找出某份正文对应的路径（留证要能点名「哪个文件没取回来」）。
func pathOfSHA(ops []treeOp, sha string) string {
	for _, op := range ops {
		if op.SHA == sha && op.Path != "" {
			return op.Path
		}
	}
	return ""
}

// isBaseTreeMismatch 判断一个推送失败是不是「对端不认识我们的基准」。
//
// 为什么靠文本（而不是类型化错误）：这句话是**对端**生成的，对端可能跑着
// 旧版本，消息本身不在本端控制之内。跨机器传递的错误只能按对方实际发出的
// 那句话识别；这个判据一旦失效，代价是回落到「人工去要快照」——所以它同时
// 被测试按真实错误串钉住（见 TestPushFallsBackToSnapshotOnBaseMismatch）。
func isBaseTreeMismatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "base tree mismatch")
}

// pullOpsFromPeer 从对端拉命令，落地并补齐对象。
func (s *Server) pullOpsFromPeer(client *http.Client, baseURL, token, peerName, project, branch string) (opApplyResult, error) {
	var zero opApplyResult
	// 拉的水位：对端日志属主的名字。
	//
	// 踩过的坑（测试抓到）：这里曾用配置里的对端别名 peerName 查水位，
	// 而 ingestOps 用对端自报的 instanceID 记水位——两个键不同，
	// 于是每轮都从头拉、每轮都重复应用。键必须用日志属主名。
	peerKey := s.resolvePeer(peerName)
	// 与推方向同理：基准一变，旧位点在新流里没有意义。
	// 不抹会让「数字恰好对得上」的那种错位把拉取永久卡住。
	if s.workSync != nil && s.opLogs != nil {
		if l, lerr := s.opLogs.For(project, branch); lerr == nil {
			if s.workSync.baseOf(project, branch, peerKey) != l.BaseTree() {
				s.workSync.dropWatermarks(project, branch, peerKey)
			}
		}
	}
	after := s.peerSeq(project, branch, peerKey)

	// 带上本地当前这段流的基准树：对端据此判断我们的流是不是挂在
	// 同一棵树上。不是同一棵树就发 NeedRebase，不瞎发增量。
	// 告诉对端「我手上是哪棵树」。取值顺序：
	//   1. 我为这个对端记下的基准（快照落地后记的就是内容树）
	//   2. 本端 oplog 的基准
	//
	// 用 1 的原因是：本端 oplog 的基准是「我这条流挂在哪棵树上」，
	// 而快照落地后我手上的内容已经跑到那棵树前面去了；拿自己的原点
	// 去问，对端会以为我落后，于是每轮重发整树。
	baseTree := ""
	if s.workSync != nil {
		baseTree = s.workSync.peerTreeOf(project, branch, peerKey)
	}
	if baseTree == "" && s.opLogs != nil {
		if l, lerr := s.opLogs.For(project, branch); lerr == nil {
			baseTree = l.BaseTree()
		}
	}
	batch, err := postOpPullFrom(client, baseURL, token, project, branch, s.instanceID(), after, baseTree)
	if err != nil {
		return zero, err
	}
	// 即使这一批是空的，对端属主名也已经学到了——水位键从此稳定。
	s.learnPeerIdentity(peerName, batch.Node)
	if len(batch.Ops) == 0 {
		// 空批次也要留痕：否则「下游问上游有没有新东西」时，
		// 流水里看不到任何证据，容易误判成同步没在跑。
		s.recordSync(syncRecord{
			Dir: "pull", Peer: peerName, Node: batch.Node,
			Project: project, Branch: branch,
			From: after, To: after, PeerHead: int(batch.To),
		})
		return zero, nil
	}
	res, err := s.ingestOps(project, branch, batch)
	if err != nil {
		return res, err
	}
	if len(res.Missing) > 0 {
		seen := map[string]bool{}
		failed := map[string]string{} // sha -> 错误
		for _, sha := range res.Missing {
			if seen[sha] {
				continue
			}
			seen[sha] = true
			body, berr := getWorkBlob(client, baseURL, token, project, branch, sha)
			if berr != nil {
				// 不再整批退出（2026-11-24 群内评审第 1 条）：
				// 一个正文取不回来，其余路径不该被它拖住。
				failed[sha] = berr.Error()
				continue
			}
			if aerr := s.applyWorkBlob(project, sha, body); aerr != nil {
				failed[sha] = aerr.Error()
				continue
			}
		}
		// 对象补齐后重放：这一遍应当把已补齐的那些路径落下去。
		res, err = s.applyOps(project, branch, batch.Node, batch.Ops)
		if err != nil {
			return res, err
		}
		s.commitOpsLocal(project, branch, batch.Node, batch.Ops, res)

		// 仍然缺的那些命令：留证，并把水位钉在它之前。
		//
		// 为什么必须钉（而不是直接推进到 batch.To）：水位一过，下一轮
		// 就不会再拉这几条，丢的内容永远补不回来。回退的代价是尾部重拉
		// 一遍，而 applyOp 对同内容已经幂等跳过（不重写、不重刷、不重广播）。
		if len(res.Missing) > 0 {
			pin := firstBlockedSeq(batch.Ops, res.Missing)
			for _, sha := range res.Missing {
				if seen[sha] && failed[sha] == "" {
					continue // 已经补回来了，还在 Missing 里只是重放顺序问题
				}
				s.workSync.noteBlocked(blockedBlob{
					Project: project, Branch: branch, Peer: batch.Node,
					Path: pathOfSHA(batch.Ops, sha), SHA: sha,
					Seq: seqOfSHA(batch.Ops, sha), At: time.Now(),
					LastErr: failed[sha],
				})
			}
			if pin > 0 {
				forceWatermark(&s.workSync.in, workKey(project, branch, batch.Node), pin-1)
				s.logf("op-sync: %s@%s: %d 条命令缺正文，已把水位钉在 seq %d（其余命令已落地）",
					project, branch, len(res.Missing), pin-1)
			}
		} else {
			if n := s.workSync.clearBlockedPeer(project, branch, batch.Node); n > 0 {
				s.logf("op-sync: %s@%s: %d 条缺正文已补齐，留证已清除", project, branch, n)
			}
		}
	}
	s.recordSync(syncRecord{
		Dir: "pull", Peer: peerName, Node: batch.Node,
		Project: project, Branch: branch,
		From: after, To: batch.To, PeerHead: int(batch.To),
		Applied: res.Applied, Deleted: res.Deleted, Kept: len(res.Kept),
		Conflicted: len(res.Conflicted), Missing: len(res.Missing),
		Rejected: len(res.Rejected),
	})
	if serr := s.saveOpSyncState(); serr != nil {
		s.logf("opsync: save watermark after pull: %v", serr)
	}
	return res, nil
}

// syncOpsWithRemote 对一个对端做完整的推 + 拉。
//
// 分支列表的缺省规则：pull 未显式配置时回落到 push 列表——
// 大多数对等配置只写一次分支名单，意图就是这几条保持双向一致，
// 不该逼人把同一组名字抄两遍。
func (s *Server) syncOpsWithRemote(client *http.Client, rm configRemote, project string) error {
	if !rm.FetchOnly {
		for _, br := range rm.Push {
			if br == "*" {
				continue
			}
			if _, err := s.pushOpsToPeer(client, rm.URL, rm.Token, rm.Name, project, br); err != nil {
				return fmt.Errorf("push %s: %w", br, err)
			}
		}
	}
	// 取回哪些分支与 Git 层同一判据，不再各自算一份。
	for _, br := range fetchBranches(rm.Pull, rm.Push, rm.PushOnly) {
		if br == "*" {
			continue
		}
		if _, err := s.pullOpsFromPeer(client, rm.URL, rm.Token, rm.Name, project, br); err != nil {
			return fmt.Errorf("pull %s: %w", br, err)
		}
	}
	return nil
}

// configRemote 是同步引擎需要的最小对端视图，避免与 config.Remote 耦合。
type configRemote struct {
	Name      string
	URL       string
	Token     string
	Push      []string
	Pull      []string
	FetchOnly bool
	// PushOnly 为真时只推不拉。
	//
	// 备份节点必须这么配：从备份回头拉，一旦对方某分支只落地了一半
	// （blob 缺失留下的 Missing），它播种的权威整树就是残缺的，
	// 接收端会把「批次里没有」的路径全部墓碑。
	PushOnly bool
}

// opSyncLines 是 status 里的命令流水位报告。
func (s *Server) opSyncLines(c Caller) []string {
	if s.opLogs == nil {
		return nil
	}
	var out []string
	s.opLogs.logs.Range(func(key, v any) bool {
		k := key.(string)
		parts := strings.SplitN(k, "\x00", 2)
		if len(parts) != 2 {
			return true
		}
		project, branch := parts[0], parts[1]
		if !s.allowed(c, project, branch, configBitRead) {
			return true
		}
		l := v.(*opLog)
		line := fmt.Sprintf("oplog %s@%s: %d op(s), head seq %d", project, branch, l.Len(), l.Last())
		// 锚点水位必须可见：它是「从某个提交增量同步」能不能成立的前提。
		// 看不到它，就分不出「没增量」和「没有基准」这两种完全不同的状态。
		if bt := l.BaseTree(); bt != "" && bt != gitstore.EmptyTreeSHA {
			line += fmt.Sprintf(", base tree %s", shortSHA(bt))
		}
		out = append(out, line)
		return true
	})
	// 缺正文留证：这是「同步为什么没走完」的直接答案，必须能被看到。
	//
	// 为什么放在这里（2026-11-24 群内评审第 1 条）：早先它什么都不说——
	// 一个 blob 取不回来，整批静默退回去，从外面只能看到「水位不动」。
	// 那是一种最贵的故障：有人会在错误的前提上盖三层楼。
	if s.workSync != nil {
		blocked := s.workSync.blockedAll()
		visible := blocked[:0:0]
		for _, b := range blocked {
			if s.allowed(c, b.Project, b.Branch, configBitRead) {
				visible = append(visible, b)
			}
		}
		if len(visible) > 0 {
			out = append(out, fmt.Sprintf("op-sync: %d 条命令缺正文，同步已钉在它们之前", len(visible)))
			const show = 5
			for i, b := range visible {
				if i >= show {
					out = append(out, fmt.Sprintf("  … 还有 %d 条", len(visible)-show))
					break
				}
				line := fmt.Sprintf("  BLOCKED %s@%s:%s (seq %d, sha %s, %d 次)",
					b.Project, b.Branch, b.Path, b.Seq, shortSHA(b.SHA), b.Attempts)
				if b.LastErr != "" {
					line += " — " + b.LastErr
				}
				out = append(out, line)
			}
		}
	}
	return out
}

// isSnapshotBatch 判断一批命令是不是权威整树快照。
//
// 快照带 Replace 语义：本地有、而批次里没有的路径都会被墓碑。所以它
// 自带「重设基准」的能力，不要求两边基准先对齐。
func isSnapshotBatch(batch opBatch) bool {
	return len(batch.Ops) == 1 && batch.Ops[0].Kind == opTree && batch.Ops[0].Replace
}
