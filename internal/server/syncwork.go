package server

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 工作层同步（work-layer sync）。
//
// 2026-11-22 起，跨机器同步的主通道是**命令流**（见 oplog.go / opsync.go）。
// 本文件保留的是它的两个支撑部分：
//
//   - 对象搬运端点（/sync/work/blob、/sync/work/blob/put）：命令流只搬
//     「树的事实」，正文对象仍按内容寻址、按需取送、sha1 验真。
//   - 旧的快照式推拉函数：仅被既有行为测试使用，用来证明旧通道的
//     删除/冲突语义没有被新实现改坏。新代码不要再调用它们。
//
// 落盘后置动作只有一份：commitOpsLocal（opapply.go）。
//
// 与传统 Git 同步的分工：
//
//	Git 同步     搬 commit。同步的前提是「先提交」。
//	工作层同步   搬**尚未提交的改动**。不需要 commit，因此两台机器
//	             写完就能互相看见，而历史仍然保持干净。
//
// 顺序是硬约束：**先 tree，后 blob**。
//
//	① 对端先给出「哪些路径变成什么」（tree 事实）
//	② 本地先应用这个终局：新增/修改只登记 (mode, sha)，删除立刻生效
//	③ 然后才去要「终局里仍然存在」的 blob
//
// 	这个顺序消灭了「离线节点复活已删内容」：墓碑先落地，那个 blob
// 	从此不在终局里，本地根本不会去请求它，机器上也不会有。
//
// 为什么不做成第二套对外协议：这是**服务对服务**的通道，Agent 侧
// 永远只见 MCP。端点挂在同一个 mux、走同一套鉴权与 ACL。

// workChange 是一条工作层改动（tree 事实，不含正文）。
type workChange struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"`
	// SHA 是 blob 哈希；Deleted 为真时为空。
	SHA string `json:"sha,omitempty"`
	// Deleted 为真表示这是一条墓碑。
	Deleted bool `json:"deleted,omitempty"`
}

// workDelta 是一次工作层拉取的响应。
type workDelta struct {
	Project string       `json:"project"`
	Branch  string       `json:"branch"`
	Node    string       `json:"node"`
	Changes []workChange `json:"changes"`
	// NeedBlobs 是「请求方可能还缺」的对象，供第二趟按需索取。
	NeedBlobs []string `json:"need_blobs,omitempty"`
}

// workApplyResult 是本地应用一份 delta 的结果。
type workApplyResult struct {
	Applied  int      `json:"applied"`
	Deleted  int      `json:"deleted"`
	Missing  []string `json:"missing,omitempty"` // 需要回头去要的 blob
	Verified int      `json:"verified"`          // 回读校验通过的条数
	// Conflicted 是「本地也有未同步改动、内容不同」的路径。
	//
	// 这些路径**没有**被覆盖：保留本地版本，把对端的版本记进冲突表
	// 等人工裁决。静默覆盖是最坏的解法——被吃掉的一方毫不知情。
	Conflicted []workConflict `json:"conflicted,omitempty"`
}

// workConflict 是一条工作层冲突。
type workConflict struct {
	Path      string `json:"path"`
	LocalSHA  string `json:"local_sha"`
	RemoteSHA string `json:"remote_sha"`
	Peer      string `json:"peer"`
}

// workSyncState 保存每个 (project@branch -> peer) 的水位。
//
// outSeq 只在**对端确认之后**才推进，这是「发出了不代表真的发了」的
// 落地：本地觉得推成功没有意义，必须对方验过并回执。
type workSyncState struct {
	out sync.Map // string -> *atomic.Int64
	in  sync.Map // string -> *atomic.Int64
	// bases 是「那个水位属于哪段命令流」。
	//
	// 命令流每次提交都归零、seq 从 1 重来，所以位点只在同一段流内部
	// 可比。只记数字会出现最坏的那种对齐：旧流的 seq 1 和新流的 seq 1
	// 数字相同、含义完全不同，于是 Since(1) 返回空，同步静默停摆。
	bases sync.Map // string -> string
	// conflicts 是 project\x00branch\x00path -> workConflict。
	conflicts sync.Map
}

func newWorkSyncState() *workSyncState {
	return &workSyncState{}
}

func workKey(project, branch, peer string) string {
	return project + "\x00" + branch + "\x00" + peer
}

func (w *workSyncState) noteOut(project, branch, peer string, seq int64) {
	noteWatermark(&w.out, workKey(project, branch, peer), seq)
}

func (w *workSyncState) noteIn(project, branch, peer string, seq int64) {
	noteWatermark(&w.in, workKey(project, branch, peer), seq)
}

func noteWatermark(m *sync.Map, key string, seq int64) {
	for {
		actual, _ := m.LoadOrStore(key, new(atomic.Int64))
		cur := actual.(*atomic.Int64)
		old := cur.Load()
		if seq <= old {
			return
		}
		if cur.CompareAndSwap(old, seq) {
			return
		}
	}
}

// noteBase 记住某个对端的水位是哪段流的。
func (w *workSyncState) noteBase(project, branch, peer, base string) {
	w.bases.Store(workKey(project, branch, peer), base)
}

// baseOf 返回该对端水位所属的基准树；从未同步过则为空。
func (w *workSyncState) baseOf(project, branch, peer string) string {
	if v, ok := w.bases.Load(workKey(project, branch, peer)); ok {
		return v.(string)
	}
	return ""
}

// dropWatermarks 抹掉某对端的双向水位。
//
// 基准变了就必须抹：旧位点在新流里没有意义，留着会让「看起来对得上」
// 的数字把同步卡死。
func (w *workSyncState) dropWatermarks(project, branch, peer string) {
	k := workKey(project, branch, peer)
	if v, ok := w.in.Load(k); ok {
		v.(*atomic.Int64).Store(0)
	}
	if v, ok := w.out.Load(k); ok {
		v.(*atomic.Int64).Store(0)
	}
}

func (w *workSyncState) cursors(project, branch, peer string) (out, in int64) {
	k := workKey(project, branch, peer)
	if v, ok := w.out.Load(k); ok {
		out = v.(*atomic.Int64).Load()
	}
	if v, ok := w.in.Load(k); ok {
		in = v.(*atomic.Int64).Load()
	}
	return out, in
}

// recordWorkConflict 记下一条冲突。
func (s *Server) recordWorkConflict(project, branch, path, localSHA, remoteSHA, peer string) {
	s.workSync.conflicts.Store(project+"\x00"+branch+"\x00"+path, workConflict{
		Path: path, LocalSHA: localSHA, RemoteSHA: remoteSHA, Peer: peer,
	})
}

// clearWorkConflict 在两端重新一致后清掉冲突。
func (s *Server) clearWorkConflict(project, branch, path string) {
	s.workSync.conflicts.Delete(project + "\x00" + branch + "\x00" + path)
}

// workConflicts 列出当前未解决的冲突。
func (s *Server) workConflicts() []workConflict {
	var out []workConflict
	s.workSync.conflicts.Range(func(_, v any) bool {
		if c, ok := v.(workConflict); ok {
			out = append(out, c)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// exportWorkDelta 读出某分支当前完整的工作层改动（tree 事实）。
//
// 为什么发全量而不是增量：脏层是按目录覆盖的**状态**，不是日志。
// 状态永远可以整份重发，天然幂等；而增量需要可靠的序号分配，在
// 多写者场景下很容易出现「漏了一号就永久缺口」。先用状态换正确性，
// 需要省带宽时再叠一层指纹（对端已有则跳过）。
func (s *Server) exportWorkDelta(project, branch string) (*workDelta, error) {
	repo, err := s.projects.Get(project)
	if err != nil {
		return nil, err
	}
	changes, err := repo.DirtyChanges(branch)
	if err != nil {
		return nil, err
	}
	out := &workDelta{Project: project, Branch: branch, Node: s.instanceID()}
	out.Changes = make([]workChange, 0, len(changes))
	seen := map[string]bool{}
	for _, ch := range changes {
		wc := workChange{Path: ch.Path, Mode: string(ch.Mode), SHA: ch.SHA, Deleted: ch.Deleted}
		out.Changes = append(out.Changes, wc)
		if !ch.Deleted && ch.SHA != "" && !seen[ch.SHA] {
			seen[ch.SHA] = true
			out.NeedBlobs = append(out.NeedBlobs, ch.SHA)
		}
	}
	sort.Strings(out.NeedBlobs)
	return out, nil
}

// importWorkDelta 把一份 delta 应用到本地工作层。
//
// 这是「先 tree」那一半：只登记事实，缺的 blob 记进 Missing 由调用方
// 第二趟去取。删除在这里就已经生效——所以被删的 blob 永远不会再被请求。
func (s *Server) importWorkDelta(project, branch string, d *workDelta) (workApplyResult, error) {
	var res workApplyResult
	repo, err := s.projects.Get(project)
	if err != nil {
		return res, err
	}

	// 先把本地工作层读一份索引：判断「这条路径我这儿有没有未同步的改动」。
	//
	// 为什么必须判：没有这一步，两台机器同时改同一条记忆的结果是
	// 后到者静默覆盖先到者——被吃掉的那一方全程不知情，直到某天发现
	// 自己的话不见了。那种故障无法追，因为没有任何日志、没有任何冲突面。
	local, lerr := repo.DirtyChanges(branch)
	if lerr != nil {
		return res, fmt.Errorf("read local dirty layer: %w", lerr)
	}
	localByPath := make(map[string]gitstore.DirtyChange, len(local))
	for _, ch := range local {
		localByPath[ch.Path] = ch
	}

	for _, ch := range d.Changes {
		if ch.Deleted {
			// 墓碑先落地：不等 blob，也不需要 blob。
			//
			// 墓碑仍然优先落地，但**不许静默**。
			//
			// 两个性质要同时成立：
			//
			//	① 删除优先——离线节点不得复活已删内容（见
			//	   TestWorkLayerDeleteDoesNotResurrect）。
			//	现在本地有未同步改动就拒绝墓碑，会直接破坏它。
			//
			//	② 不得静默丢内容——全网两端并发时，
			//	  「A 删 / B 改」会让 B 的未提交内容消失。
			//	 实测（2026-11-21）：回执里全是 Applied，没有任何声音。
			//
			// 做法：墓碑照旧落地，但把被它覆盖的本地 SHA 记进冲突表
			// 并在日志里点名。丢弃不再无声，且能被 status 看见。
			if lch, ok := localByPath[ch.Path]; ok && !lch.Deleted {
				res.Conflicted = append(res.Conflicted, workConflict{
					Path:      ch.Path,
					LocalSHA:  lch.SHA,
					RemoteSHA: "",
					Peer:      d.Node,
				})
				s.recordWorkConflict(project, branch, ch.Path, lch.SHA, "", d.Node)
				s.logf("work-sync: DELETE WINS %s@%s:%s (peer %s deleted it; local uncommitted %s was unlinked — "+
					"blob still exists in the object store if you need to recover it)",
					project, branch, ch.Path, d.Node, shortSHA(lch.SHA))
			}
			if err := repo.ApplyChange(branch, ch.Path, "", "", true); err != nil {
				return res, fmt.Errorf("apply delete %s: %w", ch.Path, err)
			}
			res.Deleted++
			res.Applied++
			continue
		}
		if ch.SHA == "" || !repo.HasObject(ch.SHA) {
			// 正文还没到：记下来，第二趟再取。
			// 注意这里**不写工作层**——写一条指向空气的条目，
			// 读取时才会炸，而且爆在完全无关的地方。
			res.Missing = append(res.Missing, ch.SHA)
			continue
		}
		// 冲突面：本地对同一路径有未同步的不同内容 → 不覆盖，上报。
		//
		// 相同内容（我方也是这个 sha）不算冲突：那是同一份改动的
		// 重复投递，幂等即可。
		if lch, ok := localByPath[ch.Path]; ok && !lch.Deleted && lch.SHA != ch.SHA {
			res.Conflicted = append(res.Conflicted, workConflict{
				Path:      ch.Path,
				LocalSHA:  lch.SHA,
				RemoteSHA: ch.SHA,
				Peer:      d.Node,
			})
			s.recordWorkConflict(project, branch, ch.Path, lch.SHA, ch.SHA, d.Node)
			s.logf("work-sync: CONFLICT %s@%s:%s (local %s vs peer %s from %s) — kept local copy",
				project, branch, ch.Path, shortSHA(lch.SHA), shortSHA(ch.SHA), d.Node)
			continue
		}
		if err := repo.ApplyChange(branch, ch.Path, gitstore.Mode(ch.Mode), ch.SHA, false); err != nil {
			return res, fmt.Errorf("apply set %s: %w", ch.Path, err)
		}
		// 同路径再次达成一致：把这条冲突记录清掉。
		s.clearWorkConflict(project, branch, ch.Path)
		res.Applied++
	}
	return res, nil
}

// applyWorkBlob 落一个对端送来的 blob，并**回读校验**。
//
// 校验不通过就拒绝落盘：宁可这一条同步失败并在 status 里报出来，
// 也不让一个内容与名字不符的对象进入库——那种损坏之后会在任何一次
// 读取时表现为「memory 不见了」，极难追。
func (s *Server) applyWorkBlob(project string, sha string, body []byte) error {
	repo, err := s.projects.Get(project)
	if err != nil {
		return err
	}
	sum := sha1.Sum(prefixedBlob(body))
	if got := hex.EncodeToString(sum[:]); got != sha {
		return fmt.Errorf("blob %s failed read-back verification (content hashes to %s)", sha, got)
	}
	if repo.HasObject(sha) {
		return nil
	}
	return repo.PutBlob(sha, body)
}

func prefixedBlob(body []byte) []byte {
	head := fmt.Sprintf("blob %d\x00", len(body))
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	return append(out, body...)
}

// instanceID 是本节点的稳定标识（用于水位分桶与日志）。
//
// 优先用配置里的 endpoint 名，它本来就是「这台机器」的标签；
// 没有就退到监听地址，再没有就用 local。
func (s *Server) instanceID() string {
	if s.cfg.Endpoint != "" {
		return s.cfg.Endpoint
	}
	if s.cfg.Listen != "" {
		return s.cfg.Listen
	}
	return "local"
}

// handleWorkPull 供对端拉取本节点的工作层改动。
//
// 鉴权复用同一套中间件，权限判定与读取一致：能读这条分支，才能拉它
// 的工作层。工作层不是「特权通道」，它等同于读。
func (s *Server) handleWorkPull(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		Project string `json:"project"`
		Branch  string `json:"branch"`
		Node    string `json:"node"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad work-pull request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.allowed(c, req.Project, req.Branch, configBitRead) {
		http.Error(w, s.denyReason(c, req.Project, req.Branch, configBitRead), http.StatusForbidden)
		return
	}
	d, err := s.exportWorkDelta(req.Project, req.Branch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

// handleWorkBlob 供对端按需取一个 blob 的正文。
func (s *Server) handleWorkBlob(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	project := r.URL.Query().Get("project")
	branch := r.URL.Query().Get("branch")
	sha := r.URL.Query().Get("sha")
	if project == "" || branch == "" || sha == "" {
		http.Error(w, "work-blob requires project, branch and sha", http.StatusBadRequest)
		return
	}
	if !s.allowed(c, project, branch, configBitRead) {
		http.Error(w, s.denyReason(c, project, branch, configBitRead), http.StatusForbidden)
		return
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	body, err := repo.BlobBody(sha)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Blob-SHA", sha)
	_, _ = w.Write(body)
}

// handleWorkPush 接收对端推来的 delta（server-to-server）。
//
// 返回应用结果与「我还缺哪些 blob」，让对方第二趟补。
func (s *Server) handleWorkPush(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		Project string       `json:"project"`
		Branch  string       `json:"branch"`
		Node    string       `json:"node"`
		Seq     int64        `json:"seq"`
		Changes []workChange `json:"changes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "bad work-push request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.allowed(c, req.Project, req.Branch, configBitWrite) {
		http.Error(w, s.denyReason(c, req.Project, req.Branch, configBitWrite), http.StatusForbidden)
		return
	}
	res, err := s.importWorkDelta(req.Project, req.Branch, &workDelta{
		Project: req.Project, Branch: req.Branch, Node: req.Node, Changes: req.Changes,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 索引热插拔：把刚刚落地的路径立即反映到内存索引，
	// 让 recall 在本轮就能看见——不必等 commit，也不必重启。
	//
	// 只处理**真正落地**的路径：冲突的路径本地内容没变，
	// 既不该刷新索引，也不该广播「它变了」——那会让订阅方
	// 去取一份并不存在的新内容。
	conflicted := make(map[string]bool, len(res.Conflicted))
	for _, cf := range res.Conflicted {
		conflicted[cf.Path] = true
	}
	for _, ch := range req.Changes {
		if conflicted[ch.Path] {
			continue
		}
		exists := !ch.Deleted && s.repoHas(req.Project, req.Branch, ch.Path)
		_ = s.refreshPath(req.Project, req.Branch, ch.Path, exists)
		// 落地即广播：订阅了这棵树的 MCP 客户端（分身插件）立刻收到
		// notifications/resources/updated。工作层同步因此同时是**通讯**
		// 通道——不必等到有人 commit。
		s.publishTreeChange(req.Project, req.Branch, ch.Path, "", "",
			EventOrigin{Identity: "peer:" + req.Node, Endpoint: s.instanceID()})
	}
	s.workSync.noteIn(req.Project, req.Branch, req.Node, req.Seq)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// handleWorkPutBlob 接收对端主动推来的一个 blob。
//
// 服务端按内容重新算一遍 SHA-1，与查询参数 sha 对不上当场拒绝（400）——
// 这是「发出了不代表真的发了」的接收端回执：只有我亲眼验过、落盘，
// 对方才能把这条水位往前挪。
func (s *Server) handleWorkPutBlob(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	project := r.URL.Query().Get("project")
	branch := r.URL.Query().Get("branch")
	sha := r.URL.Query().Get("sha")
	if project == "" || branch == "" || sha == "" {
		http.Error(w, "work-put-blob requires project, branch and sha", http.StatusBadRequest)
		return
	}
	if !s.allowed(c, project, branch, configBitWrite) {
		http.Error(w, s.denyReason(c, project, branch, configBitWrite), http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.applyWorkBlob(project, sha, body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sha": sha})
}

// pullWorkFromPeer 主动从对端拉取某分支的工作层增量并应用到本地。
//
// 这是 pushWorkToPeer 的对偶：没有它，「工作层是同步主体」只成立一半。
// 上游一条未 commit 的写入，下游就只能等它 commit 后再靠传统 Git fetch
// 补上——那就退回到「必须提交才能同步」了，等于把工作层白做。
//
// 拓扑不对称：上游（常开节点）不回头连下游（IP 会变、经常离线），
// 所以「拉」必须由下游自己发起。
//
// 执行顺序与 push 对称，同样是「先 tree 后 blob」：
//
//  1. 取回改动清单。墓碑在这一趟就落地（被删路径立即消失，防复活），
//     缺正文的条目进 Missing。
//  2. 对 Missing 里的每个 blob 去对端取，本地自算 SHA-1 验真后落盘。
//  3. 第二趟应用：正文已就位，把路径登记进工作层并热插拔索引。
//  4. 每条落地路径广播 resources/updated，本机订阅者立刻看见。
func (s *Server) pullWorkFromPeer(client *http.Client, baseURL, token, peerName, project, branch string) (workApplyResult, error) {
	var zero workApplyResult
	delta, err := postWorkPull(client, baseURL, token, project, branch, s.instanceID())
	if err != nil {
		return zero, err
	}
	if len(delta.Changes) == 0 {
		return zero, nil
	}

	// 上游在 commit 之前，同一批改动会一直挂在它的工作层里，
	// 每个同步周期都会被重复列出。先剔掉本地已经一致的部分：
	// 不然下游会周期性地重写同样的 blob、重刷索引、重复广播事件。
	delta.Changes = s.dropAlreadyInSync(project, branch, delta.Changes)
	if len(delta.Changes) == 0 {
		return zero, nil
	}

	res, err := s.importWorkDelta(project, branch, &delta)
	if err != nil {
		return res, fmt.Errorf("import delta (first pass): %w", err)
	}

	if len(res.Missing) > 0 {
		seen := map[string]bool{}
		for _, sha := range res.Missing {
			if seen[sha] || sha == "" {
				continue
			}
			seen[sha] = true
			body, berr := getWorkBlob(client, baseURL, token, project, branch, sha)
			if berr != nil {
				return res, fmt.Errorf("fetch blob %s: %w", sha, berr)
			}
			if aerr := s.applyWorkBlob(project, sha, body); aerr != nil {
				return res, fmt.Errorf("apply blob %s: %w", sha, aerr)
			}
			res.Verified++
		}
		// 第二趟：正文到齐，登记路径并热插拔索引。
		res2, ierr := s.importWorkDelta(project, branch, &delta)
		if ierr != nil {
			return res, fmt.Errorf("import delta (second pass): %w", ierr)
		}
		res.Applied = res2.Applied
		res.Deleted = res2.Deleted
		res.Missing = res2.Missing
		res.Conflicted = res2.Conflicted
		if len(res.Missing) > 0 {
			return res, fmt.Errorf("still missing %d blob(s) after fetch", len(res.Missing))
		}
	}

	// 落地即广播：与 handleWorkPush 走同一套动作，
	// 否则拉下来的内容只在磁盘上，索引与订阅者都看不见。
	conflicted := make(map[string]bool, len(res.Conflicted))
	for _, cf := range res.Conflicted {
		conflicted[cf.Path] = true
	}
	for _, ch := range delta.Changes {
		if conflicted[ch.Path] {
			continue
		}
		exists := !ch.Deleted && s.repoHas(project, branch, ch.Path)
		_ = s.refreshPath(project, branch, ch.Path, exists)
		s.publishTreeChange(project, branch, ch.Path, "", "",
			EventOrigin{Identity: "peer:" + delta.Node, Endpoint: s.instanceID()})
	}
	if res.Applied > 0 || res.Deleted > 0 {
		s.workSync.noteIn(project, branch, peerName, time.Now().UnixNano())
	}
	return res, nil
}

// dropAlreadyInSync 剔除本地已经与该改动完全一致的条目。
//
// 为什么需要：上游在 commit 之前，改动会一直挂在它的工作层里。
// 下游每 30 秒轮询一次，如果不做判重，就会把相同的改动重复应用
// 几百次——每次都在解链、重写脏文件、重刷索引。
func (s *Server) dropAlreadyInSync(project, branch string, changes []workChange) []workChange {
	repo, err := s.projects.Get(project)
	if err != nil {
		return changes
	}
	out := make([]workChange, 0, len(changes))
	for _, ch := range changes {
		entry, found, serr := repo.Stat(branch, ch.Path)
		if serr != nil {
			out = append(out, ch)
			continue
		}
		if ch.Deleted {
			if found {
				out = append(out, ch)
			}
			continue
		}
		if !found || entry.Hash != ch.SHA {
			out = append(out, ch)
		}
	}
	return out
}

func postWorkPull(client *http.Client, baseURL, token, project, branch, node string) (workDelta, error) {
	var delta workDelta
	raw, err := json.Marshal(map[string]any{
		"project": project,
		"branch":  branch,
		"node":    node,
	})
	if err != nil {
		return delta, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/sync/work/pull", bytes.NewReader(raw))
	if err != nil {
		return delta, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return delta, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return delta, fmt.Errorf("work-pull HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&delta); err != nil {
		return delta, err
	}
	return delta, nil
}

// getWorkBlob 从对端取一个 blob 的正文。上限 50MB：
// 超过这个体积的单个切片不可能是记忆，多半是配置写错或对端异常。
func getWorkBlob(client *http.Client, baseURL, token, project, branch, sha string) ([]byte, error) {
	u := fmt.Sprintf("%s/sync/work/blob?project=%s&branch=%s&sha=%s",
		strings.TrimRight(baseURL, "/"), url.QueryEscape(project), url.QueryEscape(branch), url.QueryEscape(sha))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("work-blob HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 50<<20))
}

// pushWorkToPeer 把本节点某分支的工作层推送给对等体。
//
// 严格执行「先 tree，后 blob，最后落水位」三步：
//
//  1. 第一趟 push 只送改动清单（不含正文）。对端先让删除生效，
//     然后回报「我还缺哪几个 blob」——已删路径的 blob 根本不会
//     出现在这个清单里（离线节点永不复活已删内容）。
//  2. 逐个送出缺失的 blob，对端每收一个就自算 SHA-1 验真。
//  3. 若补过 blob，再推一次清单让对端把这些路径登记进工作层并热插拔索引。
//  4. 只有在对端 Missing 为空、且全部回执 200 OK 时，本地才推进 out_seq。
func (s *Server) pushWorkToPeer(client *http.Client, baseURL, token, peerName, project, branch string, seq int64) (workApplyResult, error) {
	var zero workApplyResult
	delta, err := s.exportWorkDelta(project, branch)
	if err != nil {
		return zero, err
	}
	res, err := postWorkPush(client, baseURL, token, project, branch, s.instanceID(), seq, delta.Changes)
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
		// 第二趟：所有 blob 已到对面并验真，现在让对端把路径登记进工作层。
		res, err = postWorkPush(client, baseURL, token, project, branch, s.instanceID(), seq, delta.Changes)
		if err != nil {
			return res, err
		}
		if len(res.Missing) > 0 {
			return res, fmt.Errorf("peer still reports %d missing blob(s) after upload", len(res.Missing))
		}
	}
	s.workSync.noteOut(project, branch, peerName, seq)
	return res, nil
}

func postWorkPush(client *http.Client, baseURL, token, project, branch, node string, seq int64, changes []workChange) (workApplyResult, error) {
	var res workApplyResult
	payload := map[string]any{
		"project": project,
		"branch":  branch,
		"node":    node,
		"seq":     seq,
		"changes": changes,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return res, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/sync/work/push", bytes.NewReader(raw))
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
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return res, fmt.Errorf("work-push HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return res, err
	}
	return res, nil
}

func postWorkBlob(client *http.Client, baseURL, token, project, branch, sha string, body []byte) error {
	u := fmt.Sprintf("%s/sync/work/blob/put?project=%s&branch=%s&sha=%s",
		strings.TrimRight(baseURL, "/"), url.QueryEscape(project), url.QueryEscape(branch), url.QueryEscape(sha))
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("work-blob-put HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// repoHas 判断某个路径在仓库里（含工作层）是否存在。
func (s *Server) repoHas(project, branch, path string) bool {
	repo, err := s.projects.Get(project)
	if err != nil {
		return false
	}
	_, found, serr := repo.Stat(branch, path)
	return serr == nil && found
}

// workConflictLines 给 status 报告未解决的工作层冲突。
//
// 必须出现在 status 里而不是只写日志：冲突意味着「有两份内容都自称
// 是这条记忆」，而系统选择了保留本地——这个决定必须由人看见并裁决。
func (s *Server) workConflictLines(c Caller) []string {
	cs := s.workConflicts()
	if len(cs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cs)+1)
	out = append(out, fmt.Sprintf("%d unresolved work-layer conflict(s), local copy kept:", len(cs)))
	for i, cf := range cs {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  ... (%d more)", len(cs)-i))
			break
		}
		out = append(out, fmt.Sprintf("  %s: kept local %s, peer %s offered %s",
			cf.Path, shortSHA(cf.LocalSHA), cf.Peer, shortSHA(cf.RemoteSHA)))
	}
	return out
}

// workSyncLines 给 status 报告每个对等体推进到哪了。
//
// out 只在对方确认之后才前进，所以「out 落后」本身就是一句实话：
// 还有东西没到对面。
func (s *Server) workSyncLines(c Caller) []string {
	cf := s.cfgFilePtr.Load()
	if cf == nil || !c.Mount.Empty() {
		return nil
	}
	var out []string
	for _, rm := range cf.Remotes {
		if !rm.AutoSync {
			continue
		}
		// Push 与 Pull 取并集：只拉不推的远端也必须被报出来。
		//
		// 早先这里只遍历 Push，于是一条「只配了 pull」的下游远端
		// 在 status 里完全不出现——看起来没在同步，实际在默默拉，
		// 或者反过来根本没跑。两种都无从区分。
		branches := make([]string, 0, len(rm.Push)+len(rm.Pull))
		seen := map[string]bool{}
		for _, br := range append(append([]string{}, rm.Push...), rm.Pull...) {
			if br == "*" || seen[br] {
				continue
			}
			seen[br] = true
			branches = append(branches, br)
		}
		for _, br := range branches {
			o, i := s.workSync.cursors(rm.Project, br, rm.Name)
			if o == 0 && i == 0 {
				continue
			}
			out = append(out, fmt.Sprintf("%s@%s <-> %s: out seq %d, in seq %d",
				rm.Project, br, rm.Name, o, i))
		}
	}
	return out
}

// WorkDelta 是对外可见的别名，供测试与后续动词复用。
type WorkDelta = workDelta

// ExportWorkDeltaForTest 暴露给同包测试。
func (s *Server) ExportWorkDeltaForTest(project, branch string) (*WorkDelta, error) {
	return s.exportWorkDelta(project, branch)
}

// ImportWorkDeltaForTest 暴露给同包测试。
func (s *Server) ImportWorkDeltaForTest(project, branch string, d *WorkDelta) (workApplyResult, error) {
	return s.importWorkDelta(project, branch, d)
}
