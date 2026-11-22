package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 同步流水（sync journal）。
//
// 为什么它属于记忆体而不是日志文件：
//
//	「上游现在到哪了、下游有没有把我推过去」是**记忆体自身的状态**，
//	不是运维细节。agent 问「记忆体什么情况」时，答「本机有这个、
//	上游到 seq N、下游最后拉到 seq M」才算完整。
//	写进 stderr 日志等于只有翻机器的人看得见。
//
// 因此它以 mem 动词的形式暴露（sync 无参 = 最近的同步流水），
// 上游与下游各自的节点都能从本地读到自己的视角——两边都问同一个
// 动词，答案不同但都完整。
//
// 落盘：<data>/system/synclog.ndjson（追加写）。
// 内存里只保留最近 syncJournalKeep 条：流水会一直增长，全留在内存
// 没有意义，而磁盘上的完整历史供事后追查。

// syncJournalKeep 是内存中保留的流水条数。
const syncJournalKeep = 500

// syncRecord 是一轮同步的记录。
type syncRecord struct {
	TS int64 `json:"ts"`
	// Dir 是方向：pull（从对端取）或 push（推给对端）。
	Dir string `json:"dir"`
	// Peer 是配置里的对端名。
	Peer string `json:"peer"`
	// Node 是对端自报的日志属主名（学到之后才非空）。
	Node    string `json:"node,omitempty"`
	Project string `json:"project"`
	Branch  string `json:"branch"`
	// From/To 是本端这轮处理到的位点区间。
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// PeerHead 是对端日志的最新位点（拉取时由对端批次给出）。
	//
	// 它的用途：下游据此知道自己离上游还差多少（PeerHead - To），
	// 而上游不需要主动知道下游的状态——下游会来问。
	PeerHead   int `json:"peer_head,omitempty"`
	Applied    int `json:"applied"`
	Deleted    int `json:"deleted"`
	Kept       int `json:"kept"`
	Conflicted int `json:"conflicted"`
	Missing    int `json:"missing"`
	// Err 非空表示这一轮失败。失败必须留痕：静默失败是最贵的故障。
	Err        string `json:"err,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// syncJournalState 是不可变快照。
type syncJournalState struct {
	records []syncRecord
}

// syncJournal 是同步流水：内存快照 + 追加落盘。
type syncJournal struct {
	file  string
	state atomic.Pointer[syncJournalState]
	wmu   chan struct{}
	// lastSeen 记每个 (方向,对端,项目,分支) 最近一次确认到的位点与时间。
	// 空轮询只更新它，不写流水。
	lastSeen sync.Map // string -> lastPoll
}

func newSyncJournal(file string) *syncJournal {
	j := &syncJournal{file: file, wmu: make(chan struct{}, 1)}
	j.state.Store(&syncJournalState{})
	return j
}

func (j *syncJournal) snapshot() *syncJournalState { return j.state.Load() }

// Load 读入落盘的流水，只保留最近 keep 条。
func (j *syncJournal) Load(keep int) error {
	if keep <= 0 {
		keep = syncJournalKeep
	}
	f, err := os.Open(j.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	var all []syncRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		txt := strings.TrimSpace(sc.Text())
		if txt == "" {
			continue
		}
		var rec syncRecord
		if err := json.Unmarshal([]byte(txt), &rec); err != nil {
			// 流水损坏不该拦住启动：它是诊断材料，不是真相。
			// 但要出声，否则「流水没了」会变成无声的失忆。
			return fmt.Errorf("synclog %s: %w", j.file, err)
		}
		all = append(all, rec)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(all) > keep {
		all = all[len(all)-keep:]
	}
	j.state.Store(&syncJournalState{records: all})
	return nil
}

// empty 判断这一轮是否「什么都没发生」。
//
// 后台每 2 秒轮询一次，空轮询若全记进流水，流水会被刷成噪音，
// 真正有内容的那几轮反而看不见。空轮询只更新 lastPoll，不写流水。
func (r syncRecord) empty() bool {
	return r.Applied == 0 && r.Deleted == 0 && r.Kept == 0 &&
		r.Conflicted == 0 && r.Missing == 0 && r.Err == "" &&
		r.From == r.To && int64(r.PeerHead) <= r.From
}

// lastPoll 记下「最近一次确认过对端状态」的时间，供 status 显示。
type lastPoll struct {
	TS   int64
	Head int64
}

// Record 追加一条流水。
//
// 空轮询（对端没有任何新东西）不写流水，只更新 lastPoll：
// 保留「我一直在看」的证据，同时不让流水被噪音淹没。
func (j *syncJournal) Record(rec syncRecord) {
	if rec.TS == 0 {
		rec.TS = time.Now().Unix()
	}
	key := rec.Dir + "\x00" + rec.Peer + "\x00" + rec.Project + "\x00" + rec.Branch
	if rec.empty() {
		if prev, ok := j.lastSeen.Load(key); ok {
			p := prev.(lastPoll)
			if p.Head == int64(rec.PeerHead) {
				j.lastSeen.Store(key, lastPoll{TS: rec.TS, Head: int64(rec.PeerHead)})
				return
			}
		}
		j.lastSeen.Store(key, lastPoll{TS: rec.TS, Head: int64(rec.PeerHead)})
		return
	}
	j.lastSeen.Store(key, lastPoll{TS: rec.TS, Head: int64(rec.PeerHead)})
	done, err := tokenAcquire(j.wmu, "synclog")
	if err != nil {
		return
	}
	defer done()

	line, merr := json.Marshal(rec)
	if merr == nil {
		if mkerr := os.MkdirAll(filepath.Dir(j.file), 0o755); mkerr == nil {
			if f, oerr := os.OpenFile(j.file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); oerr == nil {
				_, _ = f.Write(append(line, '\n'))
				_ = f.Close()
			}
		}
	}

	cur := j.snapshot()
	next := make([]syncRecord, 0, len(cur.records)+1)
	next = append(next, cur.records...)
	next = append(next, rec)
	if len(next) > syncJournalKeep {
		next = next[len(next)-syncJournalKeep:]
	}
	j.state.Store(&syncJournalState{records: next})
}

// Recent 返回最近 n 条，最新在前。
func (j *syncJournal) Recent(n int) []syncRecord {
	cur := j.snapshot()
	if n <= 0 || n > len(cur.records) {
		n = len(cur.records)
	}
	out := make([]syncRecord, 0, n)
	for i := len(cur.records) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, cur.records[i])
	}
	return out
}

// All 返回全部内存流水（旧在前）。
func (j *syncJournal) All() []syncRecord {
	cur := j.snapshot()
	out := make([]syncRecord, len(cur.records))
	copy(out, cur.records)
	return out
}

// recordSync 把一轮同步记进流水。
func (s *Server) recordSync(rec syncRecord) {
	if s.syncLog == nil {
		return
	}
	s.syncLog.Record(rec)
}

// ── 节点拓扑视图 ──────────────────────────────────────────────
//
// 用户要的不是「流水」，是「我这个节点现在什么位置」：
//
//	我的上游是谁、它到哪了、我落后多少
//	我的下游有谁、各自追到哪了、谁掉队了
//
// 流水是时间序列（发生过什么），拓扑是当前快照（现在什么位置）。
// 两者都要，但先回答拓扑：问「记忆体什么情况」时，人想知道的是
// 「我落后了没有、下游跟上了没有」，不是「两秒前轮询了一次」。

// syncPeerView 是一个对端在当前时刻的位置。
type syncPeerView struct {
	Name     string `json:"name"`
	Node     string `json:"node,omitempty"`
	Role     string `json:"role"` // upstream / downstream / peer
	Project  string `json:"project"`
	Branch   string `json:"branch"`
	LocalSeq int64  `json:"local_seq"`
	PeerSeq  int64  `json:"peer_seq"`
	Behind   int64  `json:"behind"`
	State    string `json:"state"` // caught up / behind / never / unknown
	LastSeen int64  `json:"last_seen,omitempty"`
	URL      string `json:"url,omitempty"`
}

// syncVerbNoArgs 是 `sync`（不带参数）的输出：本节点的拓扑位置。
//
// 这是 mem('sync') 的落点。上游问它 → 看下游追到哪；下游问它 →
// 看自己落后多少。同一个动词，两边各自的视角。
func (s *Server) syncVerbNoArgs(c Caller) (TermResult, error) {
	var sb strings.Builder
	now := time.Now()

	upstreams, peers, downstreams := s.syncTopology(c)

	fmt.Fprintf(&sb, "node: %s (this instance)\n", s.instanceID())

	// 一、上游：我拉谁。
	if len(upstreams) == 0 {
		sb.WriteString("\nupstream: (none — this node is not configured to pull from anyone)\n")
	} else {
		sb.WriteString("\nupstream:\n")
		for _, v := range upstreams {
			writePeerLine(&sb, v, now)
		}
	}

	// 二、双向对端：既推又拉的节点。
	if len(peers) > 0 {
		fmt.Fprintf(&sb, "\npeers (%d):\n", len(peers))
		for _, v := range peers {
			writePeerLine(&sb, v, now)
		}
	}

	// 三、下游：谁拉我。
	if len(downstreams) == 0 {
		sb.WriteString("\ndownstreams: (none — nobody has pulled from this node yet)\n")
	} else {
		fmt.Fprintf(&sb, "\ndownstreams (%d):\n", len(downstreams))
		for _, v := range downstreams {
			writePeerLine(&sb, v, now)
		}
	}

	// 四、最近事件：只列有内容的轮次。
	//
	// 空轮询（对端没有新命令）在 Record 里已经被过滤，但从磁盘加载的
	// 历史里可能残留——这里再滤一次，否则流水会被「0 applied」刷屏。
	recs := s.syncLog.Recent(40)
	shown := 0
	head := "\nrecent events (newest first):\n"
	for _, r := range recs {
		if r.empty() {
			continue
		}
		if shown >= 8 {
			break
		}
		if shown == 0 {
			sb.WriteString(head)
		}
		shown++
		age := now.Sub(time.Unix(r.TS, 0)).Truncate(time.Second)
		arrow := "->"
		switch r.Dir {
		case "pull":
			arrow = "<-"
		case "served":
			arrow = "=>"
		case "received":
			arrow = "<="
		}
		fmt.Fprintf(&sb, "  %-10s %s %s %s@%s seq %d..%d (%d applied, %d deleted)\n",
			age.String()+" ago", arrow, r.Peer, r.Project, r.Branch, r.From, r.To, r.Applied, r.Deleted)
	}
	if shown == 0 {
		sb.WriteString("\nrecent events: (none yet — only empty polls; no command has crossed this node recently)\n")
	}

	return TermResult{
		Text: strings.TrimRight(sb.String(), "\n"),
		Structured: map[string]any{
			"node":        s.instanceID(),
			"upstreams":   upstreams,
			"peers":       peers,
			"downstreams": downstreams,
		},
	}, nil
}

// writePeerLine 渲染一条对端状态。
func writePeerLine(sb *strings.Builder, v syncPeerView, now time.Time) {
	age := ""
	if v.LastSeen > 0 {
		age = ", last seen " + now.Sub(time.Unix(v.LastSeen, 0)).Truncate(time.Second).String() + " ago"
	}
	name := v.Name
	if v.Node != "" && v.Node != v.Name {
		name = v.Name + " (" + v.Node + ")"
	}
	fmt.Fprintf(sb, "  %s  %s@%s\n", name, v.Project, v.Branch)
	switch v.State {
	case "caught up":
		fmt.Fprintf(sb, "    caught up at seq %d%s\n", v.LocalSeq, age)
	case "behind":
		fmt.Fprintf(sb, "    behind %d (local %d, peer %d)%s\n", v.Behind, v.LocalSeq, v.PeerSeq, age)
	case "never":
		fmt.Fprintf(sb, "    never synced (peer is at seq %d)%s\n", v.PeerSeq, age)
	default:
		fmt.Fprintf(sb, "    unknown position%s\n", age)
	}
}

// syncTopology 计算本节点的上游与下游位置。
//
// 判定规则：
//   - 配置里 [[remote]] 的 pull 列表包含该分支 → 它是我的上游（我拉它）
//   - 配置里 push 列表包含该分支 → 它是我的下游（我推给它）
//   - 流水里出现 served（它来拉我）→ 它是我的下游
//   - 流水里出现 received（它推给我）→ 它是我的上游
func (s *Server) syncTopology(c Caller) (upstreams, peers, downstreams []syncPeerView) {
	now := time.Now()

	// 一、从配置推导：谁是我配的上游 / 下游。
	for _, rm := range s.allRemotes() {
		if !s.allowed(c, rm.Project, "", configBitRead) {
			continue
		}
		// Git 远端（.git 结尾）走的是 commit 同步，不是命令流对端。
		// 把它们列进「上游/下游」会让节点视图失真：它们不消费命令流，
		// 也不产生命令流，用 seq 描述它们毫无意义。
		if !isWorkPeerURL(rm.URL) {
			continue
		}
		branches := map[string]bool{}
		for _, b := range rm.Pull {
			branches[b] = true
		}
		for _, b := range rm.Push {
			branches[b] = true
		}
		if len(branches) == 0 {
			branches["*"] = true
		}
		for b := range branches {
			role := ""
			isPull := containsStr(rm.Pull, b) || containsStr(rm.Pull, "*")
			isPush := containsStr(rm.Push, b) || containsStr(rm.Push, "*")
			switch {
			case isPull && isPush:
				role = "peer"
			case isPull:
				role = "upstream"
			case isPush:
				role = "downstream"
			}
			if role == "" {
				continue
			}
			v := s.peerView(rm.Project, b, rm.Name, role, now)
			v.URL = redactURL(rm.URL)
			switch role {
			case "upstream":
				upstreams = append(upstreams, v)
			case "downstream":
				downstreams = append(downstreams, v)
			case "peer":
				// 双向对端单独成一节：同时列进上下游会让人以为是两台机器。
				peers = append(peers, v)
			}
		}
	}

	// 二、从流水补充：实际连接过的节点（配置里没声明的也算）。
	//
	// 只看配置是不够的：手工 sync、临时对端、配置被改过都会让
	// 「配了什么」和「实际发生了什么」对不上。流水是事实。
	served := map[string]syncPeerView{}
	pulled := map[string]syncPeerView{}
	for _, r := range s.syncLog.All() {
		switch r.Dir {
		case "pull":
			// 我拉过它 → 它是我的上游。
			if !s.allowed(c, r.Project, r.Branch, configBitRead) {
				continue
			}
			key := r.Project + "\x00" + r.Branch + "\x00" + r.Peer
			cur, ok := pulled[key]
			if !ok || r.TS >= cur.LastSeen {
				pulled[key] = syncPeerView{
					Name: r.Peer, Node: r.Node, Role: "upstream",
					Project: r.Project, Branch: r.Branch,
					LocalSeq: r.To, PeerSeq: int64(r.PeerHead), LastSeen: r.TS,
				}
			}
			continue
		case "served", "received":
			// else 分支处理
		default:
			continue
		}
		if !s.allowed(c, r.Project, r.Branch, configBitRead) {
			continue
		}
		key := r.Project + "\x00" + r.Branch + "\x00" + r.Peer
		cur, ok := served[key]
		if !ok || r.TS > cur.LastSeen {
			// served 记录里 To 是「它拿到了哪一位」，From 是「它从哪开始要」。
			// 判它追到哪，要用 To。
			served[key] = syncPeerView{
				Name: r.Peer, Node: r.Node, Role: "downstream",
				Project: r.Project, Branch: r.Branch,
				LocalSeq: r.To, PeerSeq: r.To, LastSeen: r.TS,
			}
		}
	}
	for _, v := range served {
		// 已经由配置推导出来的不重复列。
		if hasPeer(downstreams, v.Name, v.Project, v.Branch) {
			continue
		}
		// 对端追到哪 = 流水里的 To；我这边到哪 = 本地日志头。
		v.LocalSeq = s.localOpHead(v.Project, v.Branch)
		cur := v.PeerSeq
		switch {
		case v.LocalSeq == 0:
			v.State = "never"
		case cur >= v.LocalSeq:
			v.State = "caught up"
		default:
			v.State = "behind"
			v.Behind = v.LocalSeq - cur
		}
		downstreams = append(downstreams, v)
	}
	for _, v := range pulled {
		if hasPeer(upstreams, v.Name, v.Project, v.Branch) {
			continue
		}
		switch {
		case v.PeerSeq == 0 && v.LocalSeq == 0:
			v.State = "never"
		case v.LocalSeq >= v.PeerSeq:
			v.State = "caught up"
		default:
			v.State = "behind"
			v.Behind = v.PeerSeq - v.LocalSeq
		}
		upstreams = append(upstreams, v)
	}

	sort.Slice(upstreams, func(i, j int) bool { return upstreams[i].Name < upstreams[j].Name })
	sort.Slice(downstreams, func(i, j int) bool { return downstreams[i].Name < downstreams[j].Name })
	return upstreams, peers, downstreams
}

// peerView 算出一条分支上「我 vs 对端」的位置。
func (s *Server) peerView(project, branch, peerName, role string, now time.Time) syncPeerView {
	v := syncPeerView{Name: peerName, Role: role, Project: project, Branch: branch}

	// 对端最新位点：从流水里找最近一条带 PeerHead 的记录。
	var lastRec *syncRecord
	for i := len(s.syncLog.All()) - 1; i >= 0; i-- {
		r := s.syncLog.All()[i]
		if r.Project != project || r.Branch != branch || r.Peer != peerName {
			continue
		}
		rr := r
		lastRec = &rr
		break
	}

	if role == "upstream" || role == "peer" {
		// 我拉它：本地水位是 in，对端位点来自流水。
		if s.workSync != nil {
			_, in := s.workSync.cursors(project, branch, s.resolvePeer(peerName))
			v.LocalSeq = in
		}
		if lastRec != nil {
			v.PeerSeq = int64(lastRec.PeerHead)
			v.LastSeen = lastRec.TS
			if v.PeerSeq == 0 {
				v.PeerSeq = lastRec.To
			}
		}
		switch {
		case v.PeerSeq == 0 && v.LocalSeq == 0:
			v.State = "never"
		case v.LocalSeq >= v.PeerSeq:
			v.State = "caught up"
		default:
			v.State = "behind"
			v.Behind = v.PeerSeq - v.LocalSeq
		}
		return v
	}

	// 我推它 / 它拉我：比的是「我的日志头」与「它追到哪」。
	//
	// 不能拿它自己的位点跟自己比——那永远显示 caught up，
	// 而真正要回答的是「它离我还有多远」。
	v.LocalSeq = s.localOpHead(project, branch)
	if lastRec != nil {
		v.PeerSeq = lastRec.To
		v.LastSeen = lastRec.TS
	}
	switch {
	case v.LocalSeq == 0:
		v.State = "never"
	case v.PeerSeq >= v.LocalSeq:
		v.State = "caught up"
	default:
		v.State = "behind"
		v.Behind = v.LocalSeq - v.PeerSeq
	}
	return v
}

// localOpHead 返回某分支本地命令流的最新位点。
func (s *Server) localOpHead(project, branch string) int64 {
	if s.opLogs == nil {
		return 0
	}
	l, err := s.opLogs.For(project, branch)
	if err != nil {
		return 0
	}
	return l.Last()
}

// allRemotes 返回全部远端声明（跨项目）。
func (s *Server) allRemotes() []config.Remote {
	p := s.remotes.Load()
	if p == nil {
		return nil
	}
	return *p
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func hasPeer(list []syncPeerView, name, project, branch string) bool {
	for _, v := range list {
		if v.Name == name && v.Project == project && v.Branch == branch {
			return true
		}
	}
	return false
}

// redactURL 去掉 URL 里的凭据，避免把 token 打进记忆。
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	if i := strings.Index(raw, "@"); i > 0 {
		if j := strings.Index(raw, "://"); j >= 0 && j < i {
			return raw[:j+3] + "***@" + raw[i+1:]
		}
	}
	return raw
}
