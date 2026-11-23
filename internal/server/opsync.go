package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// exportOpsFrom 在 exportOps 之上支持「以某个提交为基准」。
//
// baseCommit 回答的是位点水位回答不了的问题：对端已经通过 Git 拿到了
// 某个提交，但它本地这份日志的位点是 0——按位点它要整棵树（几百 MB），
// 而它真正缺的只是那个提交之后的那几条命令。
//
// 找不到基准时不报错，而是退化为整树：报错会让老客户端永远同步不了，
// 而整树恰好是「没有基准」时唯一正确的答案。
func (s *Server) exportOpsFrom(project, branch string, after int64, baseCommit string) (opBatch, error) {
	batch := opBatch{Project: project, Branch: branch, Node: s.instanceID(), From: after}
	if s.opLogs == nil {
		return batch, fmt.Errorf("oplog is not initialised")
	}
	l, err := s.opLogs.For(project, branch)
	if err != nil {
		return batch, err
	}

	// 对端给了基准提交：能定位到位点就只发那之后的命令。
	// 位点只进不退：对端自报的水位更新时，以它为准。
	if baseCommit != "" {
		if seq, ok := l.SeqForCommit(baseCommit); ok {
			if seq > after {
				after = seq
				batch.From = seq
			}
			batch.BaseCommit = baseCommit
		} else {
			// 出声：静默退化会让对端以为拿到了增量，而实际收到整树。
			s.logf("op-sync: %s@%s: peer asked from commit %s, no anchor here; sending full tree",
				project, branch, shortSHA(baseCommit))
		}
	}

	// 读侧播种：对端从位点 0 开始拉、而本地日志尚为空时，
	// 先把当前整棵树作为第一条批量命令播进日志。
	// 这保证了升级到命令流之前的所有历史能完整同步给对端。
	if after == 0 && l.Last() == 0 {
		if serr := l.seedIfEmpty(); serr != nil {
			s.logf("oplog: seed initial tree for %s@%s: %v", project, branch, serr)
		}
	}
	batch.Ops = l.Since(after)
	batch.To = l.Last()
	// 整树批次带上本节点最近的锚点：对端据此知道「这棵树对应哪个提交」，
	// 下次就能拿着这个提交来要增量，不必再要一遍整树。
	if batch.BaseCommit == "" && after == 0 {
		if c, _, ok := l.LatestAnchor(); ok {
			batch.BaseCommit = c
		}
	}
	return batch, nil
}

// ingestOps 把对端推来的一批命令接进本地：应用 → 补 blob → 重放。
//
// 这是 server 端 handleOpPush 与本端 pull 落地共用的入口。
func (s *Server) ingestOps(project, branch string, batch opBatch) (opApplyResult, error) {
	// 基准提交校验：对端声称这批命令是基于某个提交的增量。
	// 若本端根本没有这个提交对象，说明双方基准错位，不能闭着眼睛重放。
	// 失败必须出声：静默吞掉会导致「命令看似都 apply 了，但树基准错了，
	// 结果是一棵拼接出来的坏树」。
	if batch.BaseCommit != "" {
		if repo, err := s.projects.Get(project); err == nil {
			if !repo.HasObject(batch.BaseCommit) {
				s.logf("op-sync: %s@%s: received ops based on commit %s, but local repo lacks it; applying ops anyway (missing objects will be pulled)",
					project, branch, shortSHA(batch.BaseCommit))
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
		// BaseCommit 是可选基准：对端已经持有的提交。
		// 给了它，就只发该提交之后的命令；给不出就退化为整树。
		BaseCommit string `json:"base_commit,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad op-pull request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.allowed(c, req.Project, req.Branch, configBitRead) {
		http.Error(w, s.denyReason(c, req.Project, req.Branch, configBitRead), http.StatusForbidden)
		return
	}
	batch, err := s.exportOpsFrom(req.Project, req.Branch, req.After, req.BaseCommit)
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
func postOpPullFrom(client *http.Client, baseURL, token, project, branch, node string, after int64, baseCommit string) (opBatch, error) {
	var batch opBatch
	payload := map[string]any{"project": project, "branch": branch, "node": node, "after": after}
	if baseCommit != "" {
		payload["base_commit"] = baseCommit
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
		return res, fmt.Errorf("op-push %s: %s", resp.Status, strings.TrimSpace(string(data)))
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
	after := s.peerOutSeq(project, branch, s.resolvePeer(peerName))
	batch, err := s.exportOps(project, branch, after)
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
				return res, fmt.Errorf("read local blob %s: %w", sha, berr)
			}
			if perr := postWorkBlob(client, baseURL, token, project, branch, sha, body); perr != nil {
				return res, fmt.Errorf("push blob %s: %w", sha, perr)
			}
		}
		// 第二趟：对象已到对面并验真，现在重放同一批命令。
		res, err = postOpPush(client, baseURL, token, batch)
		if err != nil {
			return res, err
		}
		if len(res.Missing) > 0 {
			return res, fmt.Errorf("peer still reports %d missing blob(s) after upload", len(res.Missing))
		}
	}
	// 只有对端确认之后才推进本地水位。
	s.workSync.noteOut(project, branch, peerName, l.Last())
	s.recordSync(syncRecord{
		Dir: "push", Peer: peerName, Node: s.resolvePeer(peerName),
		Project: project, Branch: branch,
		From: after, To: l.Last(), PeerHead: len(batch.Ops),
		Applied: res.Applied, Deleted: res.Deleted, Kept: len(res.Kept),
		Conflicted: len(res.Conflicted), Missing: len(res.Missing),
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
	after := s.peerSeq(project, branch, s.resolvePeer(peerName))

	// 若本端从来没从该对端拉过（after=0），但本地这条分支已经有一个提交
	// （比如通过 Git 浅拷贝或 fetch 刚拉下来的），把这个提交带过去：
	// 对端如果也认得这个提交，就只发该提交之后的增量，不必把整棵树重发一遍。
	baseCommit := ""
	if after == 0 {
		if repo, err := s.projects.Get(project); err == nil {
			if head, herr := repo.Resolve(branch); herr == nil && head != "" {
				baseCommit = head
			}
		}
	}

	batch, err := postOpPullFrom(client, baseURL, token, project, branch, s.instanceID(), after, baseCommit)
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
				return res, fmt.Errorf("fetch blob %s: %w", sha, berr)
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
		if c, seq, ok := l.LatestAnchor(); ok {
			line += fmt.Sprintf(", anchor %s @seq %d", shortSHA(c), seq)
		}
		out = append(out, line)
		return true
	})
	return out
}
