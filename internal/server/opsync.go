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
	// 对端基准可能是：
	//   cur        —— 它跟上了本端命令流的原点
	//   sentTree   —— 它收下了我们上次发的整树快照（内容树）
	// 两者都表示「它手上的内容是本端流的某个前缀」，可以发增量。
	aligned := baseTree == cur
	if !aligned && baseTree != "" && peerNode != "" && s.workSync != nil {
		if st := s.workSync.sentTreeOf(project, branch, peerNode); st != "" && st == baseTree {
			aligned = true
		}
	}
	// 声明给对端的基准就是它手上那棵：它据此校验，也据此记录。
	batch.BaseTree = cur
	batch.BaseCommit = s.commitForTree(project, branch, cur)

	// 基准对不上：位点在这里没有意义，只能发整树快照。
	//
	// 这不是异常——两个节点各自提交过，基准必然不同。整树快照带
	// Replace 语义，对端落地后自己的基准就变成这棵树，下一轮就能
	// 走增量。想省掉这一趟大传输，对端先浅拷贝 BaseCommit 再来。
	// baseTree 为空 = 对端手上什么都没有。这种对端同样需要整树快照，
	// 除非本端自己也是空的（那就没什么可给）。
	// 只有当对端**明确给出了基准**且对不上时，才发整树快照。
	// baseTree 为空表示调用方只是在查增量命令（如 exportOps），
	// 或者第一次同步、基准尚未建立——此时走普通命令流，不盲发整树。
	if baseTree != "" && !aligned {
		entries, ferr := s.flattenTreeToOps(project, branch)
		if ferr != nil {
			return batch, ferr
		}
		// 本端一棵空树、对端却有内容：这是唯一会把对端清空的形状，
		// 必须拒绝并点名，而不是安静地发一棵空树过去。
		if len(entries) == 0 && baseTree != "" && baseTree != gitstore.EmptyTreeSHA {
			return batch, fmt.Errorf(
				"refusing to send an empty-tree snapshot for %s@%s: peer base %s is not empty, "+
					"this would erase everything on the peer", project, branch, shortSHA(baseTree))
		}
		// 内容树：对端落地后会算出同一个 SHA，并据此重设自己的基准。
		if repo, rerr := s.projects.Get(project); rerr == nil && len(entries) > 0 && s.workSync != nil && peerNode != "" {
			pathMap := make(map[string]gitstore.Entry, len(entries))
			for _, e := range entries {
				pathMap[e.Path] = gitstore.Entry{Path: e.Path, Mode: gitstore.Mode(e.Mode), Hash: e.SHA}
			}
			if tree, werr := repo.WriteTreeFromPaths(pathMap); werr == nil && tree != "" {
				s.workSync.noteSentTree(project, branch, peerNode, tree)
			}
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

	// 读侧播种：对端从位点 0 开始拉、而本地日志尚为空且基准是空树时，
	// 先把当前整棵树作为第一条批量命令播进日志。
	//
	// 注意它与上面的快照是互斥的：快照走的是 !aligned 分支（Rebase = true），
	// 播种走的是普通日志（Rebase = false）。
	// 有了 op.Rebase 区分，落地端知道这条只是普通命令，不会重设基准，
	// 于是两边对齐后不会再出现「追平后又 applied」的打架。
	if aligned && after == 0 && l.Last() == 0 && cur == gitstore.EmptyTreeSHA {
		if serr := l.seedIfEmpty(); serr != nil {
			s.logf("oplog: seed initial tree for %s@%s: %v", project, branch, serr)
		}
		batch.To = l.Last()
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
	peerBase := ""
	if s.workSync != nil {
		peerBase = s.workSync.baseOf(project, branch, peerKey)
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
	if effectivePeerBase != curBase && s.workSync != nil {
		s.workSync.dropWatermarks(project, branch, peerKey)
	}
	after := s.peerOutSeq(project, branch, peerKey)
	if after > l.Last() {
		after = 0
	}
	// exportOpsFromPeer 自己判断「对齐还是发快照」：
	// 认得出对端手上的树是本端流的某个前缀（含上次发的内容树）就发增量。
	batch, err := s.exportOpsFromPeer(project, branch, after, effectivePeerBase, peerKey)
	if err != nil {
		return zero, err
	}
	if len(batch.Ops) == 0 {
		return zero, nil
	}
	res, err := postOpPush(client, baseURL, token, batch)
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
	s.workSync.noteBase(project, branch, peerKey, batch.BaseTree)
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
		baseTree = s.workSync.baseOf(project, branch, peerKey)
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
		for _, sha := range res.Missing {
			if seen[sha] {
				continue
			}
			seen[sha] = true
			body, berr := getWorkBlob(client, baseURL, token, project, branch, sha)
			if berr != nil {
				var zero opApplyResult
				return zero, fmt.Errorf("fetch blob %s: %w", sha, berr)
			}
			if aerr := s.applyWorkBlob(project, sha, body); aerr != nil {
				return res, aerr
			}
		}
		// 对象补齐后重放：这次 Missing 应为空。
		res, err = s.applyOps(project, branch, batch.Node, batch.Ops)
		if err != nil {
			return res, err
		}
		s.commitOpsLocal(project, branch, batch.Node, batch.Ops, res)
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
	pullBranches := rm.Pull
	if len(pullBranches) == 0 && !rm.PushOnly {
		pullBranches = rm.Push
	}
	for _, br := range pullBranches {
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
	return out
}

// isSnapshotBatch 判断一批命令是不是权威整树快照。
//
// 快照带 Replace 语义：本地有、而批次里没有的路径都会被墓碑。所以它
// 自带「重设基准」的能力，不要求两边基准先对齐。
func isSnapshotBatch(batch opBatch) bool {
	return len(batch.Ops) == 1 && batch.Ops[0].Kind == opTree && batch.Ops[0].Replace
}
