package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 命令流（tree op log）——跨机器一致性的唯一内部抽象。
//
// 设计（2026-11-22 定稿）：
//
//	第 0 层  内容寻址对象库（blob store）：HasObject / BlobBody / 验真。
//	第 1 层  树的命令流同步：一条命令 = 一次对树的操作，带单调位点，
//	          可增量、可去重、可重放。删除也是一条命令，不是特例。
//	第 2 层  blob 自动补齐：完全由第 1 层的 Missing 驱动，验真后重放
//	          被卡住的那条命令。
//	第 3 层  历史 = 对树做完整同步（批量命令）：一棵对端有而本地没有的
//	          树，在命令流里就是一批 set 节点。
//
// 对外与普通 Git 仓库对接时，才在边界上转成 Git 协议（fetch/push）。
// Git 不再是内部同步的真相——它只是出口。
//
// 为什么替换掉原来的「脏层全量快照」：
//
//	快照没有位点，每轮都得把整张表倒出来，于是要靠 dropAlreadyInSync
//	去重；丢一条改动检测不出来；删除要单独走一条特例分支。
//	命令流天然幂等（同一位点重放无效果）、天然增量（只发位点之后的）、
//	天然可检测缺口（位点不连续就是对端漏了）。
//
// 落盘：<data>/system/oplog/<node>/<project>/<branch>.ndjson
//
//	每行一条命令。追加写 + fsync，重启后从最后一行的 seq 续编号。
//	文件只追加不原地改：这是日志，不是状态表。

// opKind 是树命令的种类。
type opKind string

const (
	// opSet 把 path 指向 sha（新增或覆盖）。
	opSet opKind = "set"
	// opDelete 给 path 立墓碑。与 opSet 走同一条路径，不是特例。
	opDelete opKind = "delete"
	// opRename 原子改名：From → Path，内容不变。
	//
	// 为什么需要它：一次 mv 若拆成 N 条 set + M 条 delete，对端可能
	// 落到一半，看到一棵被拆散的树。rename 是一条命令，原子生效。
	opRename opKind = "rename"
	// opCopy 原子复制：From → Path。
	opCopy opKind = "copy"
	// opTree 是一条批量命令：SourceSHA 指向一棵完整的树。
	//
	// 接收端把它展开成若干条 set 再落地——这就是「历史层整树同步」
	// 在命令流里的形态：一棵树在另一台机器不存在，就是一批节点添加。
	opTree opKind = "tree"
)

// treeOp 是命令流里的一条命令。
type treeOp struct {
	// Seq 是本节点这条分支上的单调位点，从 1 开始，无缺口。
	Seq int64 `json:"seq"`
	// Node 是产生这条命令的节点 ID。
	Node string `json:"node"`
	// Ts 是产生时的 Unix 秒，仅供人看，不参与一致性判断。
	Ts   int64  `json:"ts"`
	Kind opKind `json:"kind"`
	Path string `json:"path,omitempty"`
	// From 是 rename/copy 的源路径。
	From string `json:"from,omitempty"`
	Mode string `json:"mode,omitempty"`
	SHA  string `json:"sha,omitempty"`
	// Batch 是 tree 命令展开后的批量载荷，仅 opTree 使用。
	Batch []opEntry `json:"batch,omitempty"`
	// Replace 为真表示这条批量命令是**权威的整棵树**：
	// 本地有、而批次里没有的路径，都是对端已删除的，必须一并墓碑。
	//
	// 为什么必须有它（2026-11-22 部署前发现）：只展开成 set 的话，
	// 「整树同步」只能加不能减。对端删掉的内容会在接收端活下来——
	// 正是「离线节点复活已删内容」那个不变量被破。
	Replace bool `json:"replace,omitempty"`
	// Rebase 为真表示这条整树命令是用来**重设对端基准**的（流外快照）：
	// 对端落地后要把它当作自己这段流的新原点，而不是流内的一条普通命令。
	//
	// 为什么必须区分（2026-11-23 实测）：
	//
	//	流内命令（播种，seq 1）——对端只是把内容应用上来，它的基准不变，
	//	                          后续命令继续以同一个原点编号。
	//	流外快照（Rebase）     ——对端手上的流换了原点，旧位点全部作废。
	//
	// 两者都长得像「一棵树 + Replace」，混在一起的后果是种子那条命令
	// 也被当成重设基准：对端基准跳到内容树、发送端还在原点，
	// 于是每轮都判定「基准不一致」重发整树，永远收敛不了。
	Rebase bool `json:"rebase,omitempty"`
	// Reset 为真表示这一行是「流归零」标记：它之前的命令全部作废。
	//
	// 提交发生时写入一条。之前的命令已经把内容折进了那棵树，留着它们
	// 只会让日志无限增长；而它们记的东西已经在树上，不需要重放。
	Reset bool `json:"reset,omitempty"`
	// BaseTree 仅 Reset 行使用：归零之后的流基于这棵树。
	BaseTree string `json:"base_tree,omitempty"`
}

// opEntry 是批量命令里的一项。
type opEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"`
	SHA  string `json:"sha,omitempty"`
}

// opBatch 是一次同步交换的载荷。
//
// 基准用**树**而不是提交：空树是一个真实的 SHA，于是「分支还没有任何
// 提交」也有了确定的基准，不存在「不知道基准」这个第三种状态。
type opBatch struct {
	Project string   `json:"project"`
	Branch  string   `json:"branch"`
	Node    string   `json:"node"`
	Ops     []treeOp `json:"ops"`
	// From 是这批命令之后的最新位点（对端水位）。
	From int64 `json:"from"`
	// To 是这批命令的最后一个位点。
	To int64 `json:"to"`
	// BaseTree 是这批命令所基于的树：Ops 的第 1 条就叠在它上面。
	//
	// 接收端的基准不是这棵树时，Ops 一律不成立——位点只在同一段流里
	// 可比，基准一变，所有位点都要重新解释。
	BaseTree string `json:"base_tree,omitempty"`
	// BaseCommit 是 BaseTree 对应的提交（供对端浅拷贝用）。
	//
	// 基准是空树时它为空：分支还没有任何提交，对端不需要拉任何东西。
	BaseCommit string `json:"base_commit,omitempty"`
	// Rebase 为真表示：这批命令落地之后，接收端要把自己的命令流基准
	// 重设到「落地后的内容树」上。
	//
	// 两种批次会带它：整树快照、树到树差分。共同点是——它们描述的差异
	// 是**相对对端手上那棵树**算出来的，落地后对端的内容就跑到本端的
	// 位置上了，旧位点、旧基准全部作废。不带这个标记，接收端会一直停在
	// 旧基准上，下一轮差分就会因为「基准对不上」被拒（实测报
	// base tree mismatch (local 1ed2dc54, peer 3cdcb72d)）。
	Rebase bool `json:"rebase,omitempty"`
	// ContentTree 是发送端在发出这批命令时的**完整内容树**：这批命令
	// 全部落地之后，接收端的内容必须等于它。
	//
	// 为什么需要它：基准树只能证明「我们从同一个点出发」，证明不了
	// 「我们到了同一个地方」。中间丢一条命令、某条被拒、正文没补齐，
	// 基准都还是对的，内容却已经不一样——而且两边都不报错。有了
	// ContentTree，接收端落地后自己算一遍就能比对：不等就出声、不记
	// 对端水位，下一轮走树到树差分补齐，而不是把错误当成同步完成。
	//
	// 发送端本来就要算这棵树（树到树差分的输入），所以这里不新增开销。
	// 只在 Rebase 批次上填：位点增量批次不带它，接收端跳过校验。
	ContentTree string `json:"content_tree,omitempty"`
	// NeedRebase 为真表示对端报的基准与本端对不上，Ops 为空。
	//
	// 对端收到它就去浅拷贝 BaseCommit；BaseCommit 也为空则说明两边
	// 已分叉（一边有提交一边没有），必须人工裁决。
	NeedRebase bool `json:"need_rebase,omitempty"`
}

// opLog 是一条 (node, project, branch) 的追加式命令日志。
//
// 全局无锁：读者 atomic.Load 快照；写者由一槽 channel 令牌串行化，
// 带超时可退。这与 TreeWatchHub 用的是同一套并发纪律。
type opLog struct {
	dir  string // <data>/system/oplog/<node>/<project>/
	file string // <dir>/<branch>.ndjson

	state atomic.Pointer[opLogState]
	wmu   chan struct{}
	// seed 在日志为空时给出「当前整棵树」的条目，用来登记一条批量命令。
	//
	// 为什么需要它：升级到命令流之前写入过的内容不在日志里。若不播种，
	// 对端从位点 0 拉只会拿到「升级之后的新写入」——历史整段缺失，
	// 而且两边都不报错。这正是「整树 = 批量命令」要解决的情形：
	// 一棵对端没有的树，在命令流里就是一条 opTree。
	seed func() ([]opEntry, error)
}

// opLogState 是日志的不可变快照。
type opLogState struct {
	// ops 是当前这段流的全部命令，按 Seq 升序，Seq 从 1 开始。
	ops []treeOp
	// last 是最后一条的位点。
	last int64
	// baseTree 是这段流所基于的树。
	//
	// 每次提交后归零：提交把命令折进树，流从新的基准重新从 seq 1 开始。
	// 不归零的话流会无限增长，而它记的大部分内容已经变成历史。
	baseTree string
}

func newOpLog(dir, file string) *opLog {
	l := &opLog{dir: dir, file: file, wmu: make(chan struct{}, 1)}
	l.state.Store(&opLogState{baseTree: gitstore.EmptyTreeSHA})
	return l
}

func (l *opLog) snapshot() *opLogState { return l.state.Load() }

// Last 返回当前最后一个位点。
func (l *opLog) Last() int64 { return l.snapshot().last }

// Len 返回命令条数。
func (l *opLog) Len() int { return len(l.snapshot().ops) }

// Since 取出位点大于 after 的全部命令。
//
// 调用方拿它做增量同步：只发对端还没见过的那部分。
func (l *opLog) Since(after int64) []treeOp {
	st := l.snapshot()
	if after >= st.last {
		return nil
	}
	// 位点连续时可以直接按下标切片；但日志可能从外部加载过，
	// 不赌连续性，逐条过滤更稳。
	out := make([]treeOp, 0, len(st.ops))
	for _, op := range st.ops {
		if op.Seq > after {
			out = append(out, op)
		}
	}
	return out
}

// All 返回全部命令的副本。
func (l *opLog) All() []treeOp {
	st := l.snapshot()
	out := make([]treeOp, len(st.ops))
	copy(out, st.ops)
	return out
}

// acquire 拿写令牌，带超时。与 watch/fanout 共用同一套令牌纪律。
func (l *opLog) acquire() (func(), error) { return tokenAcquire(l.wmu, "oplog") }

// Load 从磁盘读入整份日志，重建内存快照并续上位点。
//
// 损坏的行不静默跳过：报错并指出第几行。静默跳过会让位点出现
// 看不见的缺口，之后所有增量同步都建立在错误的基线上。
func (l *opLog) Load() error {
	f, err := os.Open(l.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	var ops []treeOp
	var last int64
	baseTree := gitstore.EmptyTreeSHA
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		txt := strings.TrimSpace(sc.Text())
		if txt == "" {
			continue
		}
		var op treeOp
		if err := json.Unmarshal([]byte(txt), &op); err != nil {
			return fmt.Errorf("oplog %s line %d: %w", l.file, line, err)
		}
		// 归零行：之前的命令全部作废，之后的命令基于 BaseTree 重新编号。
		if op.Reset {
			if op.BaseTree == "" {
				return fmt.Errorf("oplog %s line %d: reset without base_tree", l.file, line)
			}
			ops = nil
			last = 0
			baseTree = op.BaseTree
			continue
		}
		if op.Seq <= last {
			return fmt.Errorf("oplog %s line %d: seq %d does not advance past %d (log is corrupt)",
				l.file, line, op.Seq, last)
		}
		ops = append(ops, op)
		last = op.Seq
	}
	if err := sc.Err(); err != nil {
		return err
	}
	l.state.Store(&opLogState{ops: ops, last: last, baseTree: baseTree})
	return nil
}

// Append 追加一条命令：分配位点、落盘、推进快照。
//
// 顺序是硬约束：先写盘成功才更新内存。反过来会让内存里出现一条
// 磁盘上不存在的命令，重启后位点回退，对端水位就对不上了。
func (l *opLog) Append(op treeOp) (treeOp, error) {
	done, err := l.acquire()
	if err != nil {
		return op, err
	}
	defer done()

	// 第一条命令之前先把当前整棵树播种成一条批量命令。
	// 必须在同一把令牌里做：否则 flatten 与 append 之间有窗口，
	// 会把一条「旧树」排在新写入之后，重放出错误终局。
	if err := l.seedLocked(); err != nil {
		return op, err
	}

	st := l.snapshot()
	op.Seq = st.last + 1
	if op.Ts == 0 {
		op.Ts = time.Now().Unix()
	}
	line, err := json.Marshal(op)
	if err != nil {
		return op, err
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return op, err
	}
	f, err := os.OpenFile(l.file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return op, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return op, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return op, err
	}
	if err := f.Close(); err != nil {
		return op, err
	}

	next := make([]treeOp, len(st.ops)+1)
	copy(next, st.ops)
	next[len(st.ops)] = op
	l.state.Store(&opLogState{ops: next, last: op.Seq, baseTree: st.baseTree})
	return op, nil
}

// seedLocked 在日志为空时登记一条整树批量命令。调用方必须已持有令牌。
//
// 只在「基准是空树」时播种：基准非空说明内容已经在基准里（那是上一次
// 提交留下的树），再播一次等于把整棵树当成一条命令重复塞进新流。
// 这是 2026-11-23 归零模型落地时踩到的坑——提交后第一次写入变成了
// 「整树 + 新写入」两条命令，而不是一条。
func (l *opLog) seedLocked() error {
	if l.seed == nil || l.snapshot().last > 0 {
		return nil
	}
	if bt := l.snapshot().baseTree; bt != "" && bt != gitstore.EmptyTreeSHA {
		return nil
	}
	entries, err := l.seed()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	_, err = l.appendLocked(treeOp{Kind: opTree, Batch: entries, Replace: true})
	return err
}

// seedIfEmpty 供读侧调用：对端来要命令而本地还没有日志时，
// 先把整棵树播种成一条命令，否则对端永远拿不到历史内容。
func (l *opLog) seedIfEmpty() error {
	done, err := l.acquire()
	if err != nil {
		return err
	}
	defer done()
	return l.seedLocked()
}

// appendLocked 写一条命令，调用方必须已持有令牌。
func (l *opLog) appendLocked(op treeOp) (treeOp, error) {
	st := l.snapshot()
	op.Seq = st.last + 1
	if op.Ts == 0 {
		op.Ts = time.Now().Unix()
	}
	line, err := json.Marshal(op)
	if err != nil {
		return op, err
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return op, err
	}
	f, err := os.OpenFile(l.file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return op, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return op, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return op, err
	}
	if err := f.Close(); err != nil {
		return op, err
	}

	next := make([]treeOp, len(st.ops)+1)
	copy(next, st.ops)
	next[len(st.ops)] = op
	l.state.Store(&opLogState{ops: next, last: op.Seq, baseTree: st.baseTree})
	return op, nil
}

// Reset 把命令流归零到一条基准树。
//
// 提交发生时调用：提交把流里的命令全部折进了那棵树，这些命令不再需要
// 重放，留着只会让日志无限增长。归零后流从 seq 1 重新开始，基准变成
// 刚提交出来的那棵树。
//
// 落盘是**原子重写**而不是追加一条标记：追加的话旧命令永远留在文件里，
// 文件只增不减，与「归零」的初衷相反。先写临时文件再 rename，中途崩溃
// 时旧文件完好——它记的是上一段流，内容已经在树上，重放幂等。
func (l *opLog) Reset(baseTree string) error {
	if baseTree == "" {
		return fmt.Errorf("oplog reset %s: empty base tree", l.file)
	}
	done, err := l.acquire()
	if err != nil {
		return err
	}
	defer done()

	line, err := json.Marshal(treeOp{Reset: true, BaseTree: baseTree, Ts: time.Now().Unix()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return err
	}
	tmp := l.file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.file); err != nil {
		return err
	}
	// 目录项也要刷：rename 本身不保证目录项已落盘。
	if err := syncDirectory(l.dir); err != nil {
		return fmt.Errorf("sync oplog dir %s: %w", l.dir, err)
	}

	l.state.Store(&opLogState{baseTree: baseTree})
	return nil
}

// BaseTree 返回当前这段流所基于的树。
//
// 空值等于空树：分支还没有任何提交。基准永远是确定的，不存在
// 「不知道基准是什么」这个第三种状态。
func (l *opLog) BaseTree() string {
	bt := l.snapshot().baseTree
	if bt == "" {
		return gitstore.EmptyTreeSHA
	}
	return bt
}

// opLogStore 管理本节点全部 (project, branch) 的命令日志。
//
// 用 sync.Map 存 *opLog：日志对象本身带并发纪律，这里只管按名取。
type opLogStore struct {
	root   string // <data>/system/oplog/<node>/
	node   string
	logs   sync.Map // "project\x00branch" -> *opLog
	seeder func(project, branch string) ([]opEntry, error)
}

func newOpLogStore(dataDir, node string, seeder func(project, branch string) ([]opEntry, error)) *opLogStore {
	return &opLogStore{
		root:   filepath.Join(dataDir, "system", "oplog", node),
		node:   node,
		seeder: seeder,
	}
}

// For 取（或建）某条分支的命令日志，并保证已从磁盘加载。
func (st *opLogStore) For(project, branch string) (*opLog, error) {
	key := project + "\x00" + branch
	if v, ok := st.logs.Load(key); ok {
		return v.(*opLog), nil
	}
	// 分支名里可能有斜杠（main.chat 不会，但 zero/team 会），
	// 直接当路径用会把一条分支拆成两级目录。用转义保住一一对应。
	dir := filepath.Join(st.root, escapeLogPart(project))
	file := filepath.Join(dir, escapeLogPart(branch)+".ndjson")
	l := newOpLog(dir, file)
	if st.seeder != nil {
		l.seed = func() ([]opEntry, error) { return st.seeder(project, branch) }
	}
	if err := l.Load(); err != nil {
		return nil, err
	}
	actual, _ := st.logs.LoadOrStore(key, l)
	return actual.(*opLog), nil
}

// escapeLogPart 把项目名/分支名转成安全的单段文件名。
//
// 用 URL path 转义：斜杠变 %2F，可读性还在，且可逆。
func escapeLogPart(s string) string {
	r := strings.NewReplacer("%", "%25", "/", "%2F", "\x00", "%00")
	return r.Replace(s)
}

// recordTreeOp 把一次本地写入登记进命令流。
//
// 这是命令流的**唯一生产者入口**：write/delete/mv/cp 都经这里。
// 同步对端时发的也是这同一份日志——不存在「同步专用」的第二套数据。
func (s *Server) recordTreeOp(project, branch string, kind opKind, path, from, mode, sha string) (treeOp, error) {
	if s.opLogs == nil {
		return treeOp{}, fmt.Errorf("oplog is not initialised")
	}
	l, err := s.opLogs.For(project, branch)
	if err != nil {
		return treeOp{}, err
	}
	return l.Append(treeOp{Node: s.instanceID(), Kind: kind, Path: path, From: from, Mode: mode, SHA: sha})
}

// recordTreeBatch 把一棵完整的树登记成一条批量命令。
//
// 用途：历史层的「整树同步」。对方没有这棵树的，本地把整棵树展开成
// 一批 set 条目，作为**一条**命令追加——对端要么整棵落地，要么整棵
// 等 blob，不会看到半棵树。
func (s *Server) recordTreeBatch(project, branch string, entries []opEntry) (treeOp, error) {
	if s.opLogs == nil {
		return treeOp{}, fmt.Errorf("oplog is not initialised")
	}
	l, err := s.opLogs.For(project, branch)
	if err != nil {
		return treeOp{}, err
	}
	return l.Append(treeOp{Node: s.instanceID(), Kind: opTree, Batch: entries})
}

// flattenTreeToOps 把一棵已提交的树摊平成批量命令的载荷。
//
// 这是「历史 = 批量命令流」的桥：commit 指向的 tree 在这里变成
// 一串节点添加，与 write 产生的单条命令进入同一个抽象。
func (s *Server) flattenTreeToOps(project, branch string) ([]opEntry, error) {
	repo, err := s.projects.Get(project)
	if err != nil {
		return nil, err
	}
	flat, err := repo.FlattenBranch(branch)
	if err != nil {
		return nil, err
	}
	out := make([]opEntry, 0, len(flat))
	for _, e := range flat {
		out = append(out, opEntry{Path: e.Path, Mode: string(e.Mode), SHA: e.SHA})
	}
	return out, nil
}

// resetOpLogToCommit 把命令流归零到刚提交出来的那棵树。
//
// 这是提交的收尾：提交把流里的命令全部折进了树，流随即从这条新基准
// 重新从 seq 1 开始。不做这一步的话命令流会一直追加下去——它记的
// 大部分内容已经是历史，而每一条都还要跟着同步给对端。
//
// 取**树**而不是取提交：树是内容，提交只是「谁在何时把这个内容包了
// 一层」。两边树相同就是同一份内容，不管提交是不是同一个。
func (s *Server) resetOpLogToCommit(project, branch string, repo *gitstore.Repo, commitSHA string) {
	if s.opLogs == nil || commitSHA == "" {
		return
	}
	tree, err := repo.TreeOfCommit(commitSHA)
	if err != nil {
		s.logf("oplog: tree of commit %s for %s@%s: %v", shortSHA(commitSHA), project, branch, err)
		return
	}
	l, err := s.opLogs.For(project, branch)
	if err != nil {
		s.logf("oplog: get log for %s@%s to reset at %s: %v", project, branch, shortSHA(commitSHA), err)
		return
	}
	// 树没动就是空提交（没有内容变化）。此时流里的命令还没进树，
	// 归零会把它们丢掉——所以只在树真的前进时才归零。
	if l.BaseTree() == tree {
		return
	}
	if err := l.Reset(tree); err != nil {
		// 出声：归零失败不会破坏数据（命令还在，重放幂等），
		// 但会让命令流继续增长，而且对端的基准迟迟对不上。
		s.logf("oplog: reset %s@%s to tree %s failed: %v", project, branch, shortSHA(tree), err)
	}
}

// opSeqFrom 解析对端报来的位点。
func opSeqFrom(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}
