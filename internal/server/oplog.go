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
}

// opEntry 是批量命令里的一项。
type opEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"`
	SHA  string `json:"sha,omitempty"`
}

// opBatch 是一次同步交换的载荷。
type opBatch struct {
	Project string   `json:"project"`
	Branch  string   `json:"branch"`
	Node    string   `json:"node"`
	Ops     []treeOp `json:"ops"`
	// From 是这批命令之后的最新位点（对端水位）。
	From int64 `json:"from"`
	// To 是这批命令的最后一个位点。
	To int64 `json:"to"`
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
	// ops 是全部命令，按 Seq 升序。
	ops []treeOp
	// last 是最后一条的位点。
	last int64
}

func newOpLog(dir, file string) *opLog {
	l := &opLog{dir: dir, file: file, wmu: make(chan struct{}, 1)}
	l.state.Store(&opLogState{})
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
	l.state.Store(&opLogState{ops: ops, last: last})
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
	l.state.Store(&opLogState{ops: next, last: op.Seq})
	return op, nil
}

// seedLocked 在日志为空时登记一条整树批量命令。调用方必须已持有令牌。
func (l *opLog) seedLocked() error {
	if l.seed == nil || l.snapshot().last > 0 {
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
	l.state.Store(&opLogState{ops: next, last: op.Seq})
	return op, nil
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
