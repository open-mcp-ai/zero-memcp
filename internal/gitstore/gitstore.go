// Package gitstore 把裸 Git 仓库当成记忆的存储层。
//
// 对象格式自己实现（object.go，纯标准库 zlib + sha1），不调用 git 命令。
//
// 为什么不再调 git：
//   - 每次写要 fork 一个进程（20~50ms），而写入是热路径
//   - 依赖机器上装着 git，而记忆服务应该是一个可以直接跑的单二进制
//
// 为什么仍然用 Git 的对象格式：
//   - 内容寻址（SHA-1）本身就是幂等与去重的实现
//   - 外面的人用 `git log` / `git clone` 依然能看这个仓库
//   - 格式简单到用标准库三十行就能写对
//
// 打包与 Pack 还原全部纯 Go 内置（repack.go / pack.go / ingest.go）：
// 松散对象通过 Repack 打包为 pack + .idx v2，并能无缝被 git fsck 校验；
// 远端推送来的 packfile 在 IngestPack 展开为松散对象或由 pack 读取器直接按索引检索。
package gitstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Entry 是一棵树里的一个文件。
type Entry struct {
	// Path 是树内路径，例如 "2026-10-29/225550-local.md"。
	Path string
	// Mode 是 tree entry 的模式。
	Mode Mode
	// Size 是 blob 的字节数。
	Size int64
	// Hash 是 blob 的 SHA-1。
	Hash string
	// Time 是最后一次改动这个文件的提交时间（Unix 秒）。
	//
	// 它是「这条记忆是什么时候写的」在 Git 里的唯一来源——
	// 树本身不带时间，blob 也不带，只有 commit 带。取不到时为零值，
	// 调用方需要容忍（比如刚写入还没提交的条目）。
	Time int64
}

// Mode 是 tree entry 的模式。只保留我们真正会写的两种。
type Mode string

const (
	// ModeContent 是普通内容：进倒排索引，按需召回。
	ModeContent Mode = "100644"
	// ModeRule 是可执行位：表示「这条是规则」，可注入系统提示。
	//
	// 用 git 的可执行位表达这件事，而不是在文件里另写一份声明，
	// 是因为权限本来就该跟着内容走——写入的那一刻由写入者决定
	// 它是内容还是规则，而不是事后靠一个扫描器去猜。
	ModeRule Mode = "100755"
)

// Repo 是一个裸仓库。
type Repo struct {
	// Path 是仓库目录（通常以 .git 结尾）。
	Path string

	// fs 与 locker 抽象底层存储与并发锁。
	// 默认使用本地磁盘与进程内锁，但可由外部注入替换。
	fs     FS
	locker Locker

	// mu 保护仓库实例自身的字段。
	mu sync.Mutex

	// writer 是单线程脏层写者。Write/Delete 只把任务投递进队列就返回；
	// Read/List/Commit 会排一个 barrier，保证此前已接受的任务先处理完。
	writerOnce sync.Once
	writerCh   chan dirtyTask
	writerMu   sync.Mutex // 串行化入队与关闭；barrier 必须排在先前写入之后
	writerSize atomic.Int64
	writerErr  error         // worker 累积的异步错误，下一次 barrier 返回并清空
	writerDown chan struct{} // worker 退出后关闭，Close 等它
	writerDead bool          // 队列已关，不再接受新投递
}

// FS 返回当前仓库使用的存储抽象。
func (r *Repo) FS() FS {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fs == nil {
		r.fs = DefaultFS()
	}
	return r.fs
}

// Locker 返回当前仓库使用的锁管理器。
func (r *Repo) Locker() Locker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locker == nil {
		r.locker = newLocalLocker()
	}
	return r.locker
}

func (r *Repo) rlockDirty() func() { return r.Locker().RLockDirty() }
func (r *Repo) lockDirty() func()  { return r.Locker().LockDirty() }
func (r *Repo) lockNode(ref, dir string) func() {
	return r.Locker().LockNode(ref, dir)
}

const (
	dirtyQueueCapacity = 256
	dirtyQueueMaxBytes = 64 << 20
)

// Open 打开一个裸仓库。目录不存在时报错，不自动创建——
// 「项目存在」是一个需要人明确决定的事，不该由一次读操作顺手造就。
func Open(path string) (*Repo, error) {
	return OpenWith(path, nil, nil)
}

// OpenWith 打开仓库并注入存储与锁实现。传 nil 使用本机默认实现。
//
// 注入必须发生在仓库开始对外服务之前：所有读写与加锁路径都从 Repo
// 上取同一对实现，中途替换会让两个写者各自持不同的锁。
func OpenWith(path string, storage FS, locker Locker) (*Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if storage == nil {
		storage = DefaultFS()
	}
	if locker == nil {
		locker = newLocalLocker()
	}
	if _, err := storage.Stat(abs); err != nil {
		return nil, fmt.Errorf("仓库 %s: %w", abs, err)
	}
	for _, want := range []string{"HEAD", "objects"} {
		if _, err := storage.Stat(filepath.Join(abs, want)); err != nil {
			return nil, fmt.Errorf("%s 不是裸仓库（缺 %s）", abs, want)
		}
	}
	r := &Repo{Path: abs, fs: storage, locker: locker}
	// 脏层本身就是持久层，重启后直接生效；这里只清掉孤儿脏层
	// （所属分支已不存在）并统计还有多少改动等着折叠。
	// 失败不影响打开：脏层文件读不出来最多让那个目录退回干净层。
	_, _ = r.Recover()
	return r, nil
}

// Init 初始化一个裸仓库。全部用标准库，不调 git。
func Init(path string) (*Repo, error) {
	return InitWith(path, nil, nil)
}

// InitWith 初始化裸仓库并注入存储与锁实现。传 nil 使用本机默认实现。
func InitWith(path string, storage FS, locker Locker) (*Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if storage == nil {
		storage = DefaultFS()
	}
	if locker == nil {
		locker = newLocalLocker()
	}
	for _, d := range []string{
		filepath.Join(abs, "objects", "info"),
		filepath.Join(abs, "objects", "pack"),
		filepath.Join(abs, "refs", "heads"),
		filepath.Join(abs, "refs", "tags"),
	} {
		if err := storage.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	// HEAD 指向默认分支
	head := filepath.Join(abs, "HEAD")
	if _, err := storage.Stat(head); err != nil {
		if err := storage.WriteFile(head, []byte("ref: refs/heads/zero\n"), 0o644); err != nil {
			return nil, err
		}
	}
	cfg := filepath.Join(abs, "config")
	if _, err := storage.Stat(cfg); err != nil {
		body := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"
		if err := storage.WriteFile(cfg, []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	desc := filepath.Join(abs, "description")
	if _, err := storage.Stat(desc); err != nil {
		_ = storage.WriteFile(desc, []byte("zero-memcp memory store\n"), 0o644)
	}
	return OpenWith(abs, storage, locker)
}

// ── refs ──────────────────────────────────────────────────────

func (r *Repo) refPath(ref string) string {
	return filepath.Join(r.Path, filepath.FromSlash(ref))
}

// Refs 列出全部 ref，按名字排序。
//
// 松散 ref（refs/ 下的文件）与打包 ref（packed-refs）合并返回。
// 只看目录是错的：git clone --bare、git gc、git pack-refs 都会把
// 分支从 refs/heads/* 文件挪进 packed-refs，目录当场变空——
// 而症状是「仓库明明有提交，服务报 0 个块」，不报错、不告警。
func (r *Repo) Refs(prefix string) ([]string, error) {
	out := map[string]bool{}

	root := filepath.Join(r.Path, "refs")
	err := r.FS().WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(r.Path, p)
		if rerr != nil {
			return nil
		}
		name := filepath.ToSlash(rel)
		if strings.HasPrefix(name, prefix) {
			out[name] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, name := range r.packedRefs() {
		if strings.HasPrefix(name, prefix) {
			out[name] = true
		}
	}

	list := make([]string, 0, len(out))
	for name := range out {
		list = append(list, name)
	}
	sort.Strings(list)
	return list, nil
}

// packedRefs 读出 packed-refs 里的 ref 名（不含 peeled 行）。
func (r *Repo) packedRefs() []string {
	b, err := r.FS().ReadFile(filepath.Join(r.Path, "packed-refs"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		if _, name, ok := strings.Cut(line, " "); ok {
			out = append(out, name)
		}
	}
	return out
}

// Resolve 把一个 ref 名解析成 commit SHA。解析不到返回空串与 nil。
func (r *Repo) Resolve(ref string) (string, error) {
	// 允许简写：zero → refs/heads/zero
	cands := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		cands = append([]string{"refs/heads/" + ref}, cands...)
	}
	for _, c := range cands {
		b, err := r.FS().ReadFile(r.refPath(c))
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if strings.HasPrefix(v, "ref: ") {
			return r.Resolve(strings.TrimSpace(strings.TrimPrefix(v, "ref: ")))
		}
		if err := validSHA(v); err == nil {
			return v, nil
		}
	}
	// 打包过的 ref 在 packed-refs 里
	if b, err := r.FS().ReadFile(filepath.Join(r.Path, "packed-refs")); err == nil {
		want := ref
		if !strings.HasPrefix(want, "refs/") {
			want = "refs/heads/" + want
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
				continue
			}
			sha, name, ok := strings.Cut(line, " ")
			if ok && name == want {
				return sha, nil
			}
		}
	}
	return "", nil
}

// commitTokenTimeout 等待同分支提交排队的上限。
//
// 提交通常是毫秒级；等 30 秒说明前一个提交遭遇了致命阻塞
// （比如底层磁盘卡死），宁可报错放弃也不要无限期挂住调用者。
const commitTokenTimeout = 30 * time.Second

// acquireCommitToken 获取某分支的提交令牌（带超时）。
func (r *Repo) acquireCommitToken(normRef string) (func(), error) {
	return r.acquireCommitTokenFor(normRef, commitTokenTimeout)
}

func (r *Repo) acquireCommitTokenFor(normRef string, timeout time.Duration) (func(), error) {
	return r.Locker().AcquireCommitToken(normRef, timeout)
}

// updateRef 原子地更新一个 ref。
func (r *Repo) updateRef(ref, sha string) error {
	p := r.refPath(ref)
	dir := filepath.Dir(p)
	if err := r.FS().MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return r.FS().WriteAtomicSync(p, []byte(sha+"\n"), 0o644)
}

// ── 读 ────────────────────────────────────────────────────────

// commitOf 解析某 ref 指向的提交。ref 为空或无提交时返回零值。
func (r *Repo) commitOf(ref string) (commitObj, error) {
	sha, err := r.Resolve(ref)
	if err != nil || sha == "" {
		return commitObj{}, err
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return commitObj{}, err
	}
	if typ != "commit" {
		return commitObj{}, fmt.Errorf("%s 不是 commit，而是 %s", sha, typ)
	}
	return parseCommit(body), nil
}

// List 列出某个分支下的全部条目（干净树 + 脏层覆盖）。
func (r *Repo) List(ref string) ([]Entry, error) {
	if err := r.syncDirtyWriter(); err != nil {
		return nil, fmt.Errorf("处理脏写入队列: %w", err)
	}
	c, err := r.commitOf(ref)
	if err != nil {
		return nil, err
	}
	baseTree := c.Tree

	// 干净层：分支可能还没有任何提交，那就从空树开始。
	out := map[string]Entry{}
	if baseTree != "" {
		if err := r.loadTree(baseTree, "", out); err != nil {
			return nil, err
		}
	}

	// 脏层：把每个被改过的目录覆盖上去。
	//
	// 只在目录粒度做覆盖，不展开成路径 map：脏文件里存的是
	// 「这个目录下的哪些名字被换成了什么」，与干净层一一对应。
	unlockDirty := r.rlockDirty()
	dirs, err := r.listDirtyDirs(ref)
	if err != nil {
		unlockDirty()
		return nil, err
	}
	for _, dir := range dirs {
		n, err := r.loadDirtyNode(ref, dir)
		if err != nil || n == nil {
			continue
		}
		prefix := ""
		if dir != "" {
			prefix = dir + "/"
		}
		// 目录被整体替换时，先把干净层里该目录下的旧条目清掉。
		for name := range n.set {
			delete(out, prefix+name)
		}
		for name, e := range n.set {
			path := prefix + name
			out[path] = Entry{
				Path: path,
				Mode: e.Mode,
				Hash: e.SHA,
				Time: r.objectTime(e.SHA),
			}
		}
		for name := range n.del {
			delete(out, prefix+name)
		}
	}
	unlockDirty()

	list := make([]Entry, 0, len(out))
	for _, e := range out {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })

	// 补上大小与提交时间
	times, _ := r.fileTimes(ref)
	for i := range list {
		if t, ok := times[list[i].Path]; ok {
			list[i].Time = t
		}
		if typ, body, err := r.readObject(list[i].Hash); err == nil && typ == "blob" {
			list[i].Size = int64(len(body))
		}
	}
	return list, nil
}

// Read 读出一个文件的全部内容（脏层优先）。
func (r *Repo) Read(ref, path string) ([]byte, error) {
	if err := r.syncDirtyWriter(); err != nil {
		return nil, fmt.Errorf("处理脏写入队列: %w", err)
	}
	normRef := normalizeRef(ref)
	dir, name := splitDirName(path)

	// ① 脏层优先：刚投递的内容也必须读得到。
	//    记忆服务最常见的动作就是「写一条，马上读回来确认」。
	unlockDirty := r.rlockDirty()
	n, err := r.loadDirtyNode(normRef, dir)
	if err != nil {
		unlockDirty()
		return nil, err
	}
	if n != nil {
		if gone := n.del[name]; gone {
			unlockDirty()
			return nil, fmt.Errorf("%s 不存在（已删除，尚未提交）", path)
		}
		if e, ok := n.set[name]; ok && e.Mode != "40000" {
			unlockDirty()
			typ, body, err := r.readObject(e.SHA)
			if err != nil {
				return nil, err
			}
			if typ != "blob" {
				return nil, fmt.Errorf("%s 不是文件，而是 %s", path, typ)
			}
			return body, nil
		}
	}
	unlockDirty()

	// ② 干净层
	c, err := r.commitOf(ref)
	if err != nil {
		return nil, err
	}
	if c.Tree == "" {
		return nil, fmt.Errorf("%s 里还没有任何内容", ref)
	}
	sha, err := r.findInTree(c.Tree, "", path)
	if err != nil {
		return nil, err
	}
	if sha == "" {
		return nil, fmt.Errorf("%s 不存在", path)
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return nil, err
	}
	if typ != "blob" {
		return nil, fmt.Errorf("%s 不是文件，而是 %s", path, typ)
	}
	return body, nil
}

// splitDirName 把树内路径拆成 (目录, 名字)。根目录返回 ""。
func splitDirName(path string) (string, string) {
	path = strings.Trim(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i], path[i+1:]
	}
	return "", path
}

// findInTree 在树里按路径找 blob 的 SHA。
func (r *Repo) findInTree(treeSHA, prefix, want string) (string, error) {
	typ, body, err := r.readObject(treeSHA)
	if err != nil {
		return "", err
	}
	if typ != "tree" {
		return "", fmt.Errorf("对象 %s 不是 tree", treeSHA)
	}
	items, err := parseTree(body)
	if err != nil {
		return "", err
	}
	head, rest, hasSlash := strings.Cut(want, "/")
	for _, it := range items {
		if it.name != head {
			continue
		}
		if !hasSlash {
			if it.mode == "40000" {
				return "", fmt.Errorf("%s 是目录", want)
			}
			return it.sha, nil
		}
		if it.mode != "40000" {
			return "", fmt.Errorf("%s 不是目录", head)
		}
		return r.findInTree(it.sha, prefix+head+"/", rest)
	}
	return "", nil
}

// ReadMany 批量读取。
func (r *Repo) ReadMany(ref string, paths []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(paths))
	for _, p := range paths {
		b, err := r.Read(ref, p)
		if err != nil {
			return nil, err
		}
		out[p] = b
	}
	return out, nil
}

// ── 写 ────────────────────────────────────────────────────────

type dirtyTask struct {
	op   byte // 'W' 或 'D'
	ref  string
	path string
	data []byte
	mode Mode
	size int64
	done chan error
}

func (r *Repo) initDirtyWriter() {
	r.writerOnce.Do(func() {
		r.writerCh = make(chan dirtyTask, dirtyQueueCapacity)
		r.writerDown = make(chan struct{})
		go r.dirtyWorker()
	})
}

func (r *Repo) dirtyWorker() {
	defer close(r.writerDown)
	for task := range r.writerCh {
		if task.done != nil {
			err := r.writerErr
			r.writerErr = nil
			task.done <- err
			continue
		}
		var err error
		if task.op == 'W' {
			err = r.applyDirtyWrite(task.ref, task.path, task.data, task.mode)
		} else if task.op == 'D' {
			err = r.applyDirtyDelete(task.ref, task.path)
		}
		if err != nil && r.writerErr == nil {
			r.writerErr = err
		}
		if task.size > 0 {
			r.writerSize.Add(-task.size)
		}
	}
}

func (r *Repo) syncDirtyWriter() error {
	r.writerMu.Lock()
	if r.writerDead {
		down := r.writerDown
		r.writerMu.Unlock()
		if down != nil {
			<-down
		}
		// 已关闭的 worker 已经把队列排空了，磁盘脏层可直接读取；
		// 关闭时的异步错误由 Close 返回，不重复污染只读操作。
		return nil
	}
	r.initDirtyWriter()
	ch := make(chan error, 1)
	r.writerCh <- dirtyTask{done: ch}
	r.writerMu.Unlock()
	return <-ch
}

// Close 停止单线程写者：拒绝新投递、排空已接受的队列、等待落盘完成。
//
// 为什么不复用 commit 的分支令牌：Close 是全仓库生命周期事件，
// 与某条分支的提交排队无关；它要做的只是「队列关闸、存量刷完、
// goroutine 退出」。幂等：重复调用与「从未写过」都安全。
func (r *Repo) Close() error {
	r.writerMu.Lock()
	if r.writerDead {
		down := r.writerDown
		r.writerMu.Unlock()
		if down != nil {
			<-down
		}
		return nil
	}
	r.writerDead = true
	if r.writerCh == nil {
		r.writerMu.Unlock()
		return nil
	}
	down := r.writerDown
	close(r.writerCh)
	r.writerMu.Unlock()
	<-down
	// worker 已退出，不会再改 writerErr；返回关闭前尚未被 barrier
	// 观察到的最后一个异步写盘错误。
	return r.writerErr
}

// Write 把内容投递进脏写入队列。
//
// 语义：只承诺「已投递」。不等待落盘，不等待提交，返回即表示
// 已进入单写者队列。若队列已满则报错拒绝（调用方知晓未被采纳）。
func (r *Repo) Write(ref, path string, data []byte, mode Mode) error {
	if err := validPath(path); err != nil {
		return err
	}
	if mode == "" {
		mode = ModeContent
	}

	size := int64(len(data))
	cp := make([]byte, len(data))
	copy(cp, data)

	r.writerMu.Lock()
	if r.writerDead {
		r.writerMu.Unlock()
		return fmt.Errorf("仓库 %s 已关闭，拒绝新投递", filepath.Base(r.Path))
	}
	r.initDirtyWriter()
	queuedSize := r.writerSize.Load()
	if len(r.writerCh) >= dirtyQueueCapacity || queuedSize+size > dirtyQueueMaxBytes {
		r.writerMu.Unlock()
		return fmt.Errorf("脏写入队列已满（排队 %d 条 / %d 字节），拒绝新投递；请稍后重试", len(r.writerCh), queuedSize)
	}
	r.writerSize.Add(size)
	select {
	case r.writerCh <- dirtyTask{op: 'W', ref: ref, path: path, data: cp, mode: mode, size: size}:
		r.writerMu.Unlock()
		return nil
	default:
		r.writerSize.Add(-size)
		r.writerMu.Unlock()
		return fmt.Errorf("脏写入队列已满，拒绝新投递；请稍后重试")
	}
}

func (r *Repo) applyDirtyWrite(ref, path string, data []byte, mode Mode) error {
	normRef := normalizeRef(ref)
	dir, name := splitDirName(path)

	sha, err := r.writeObject("blob", data)
	if err != nil {
		return err
	}

	unlockDirty := r.rlockDirty()
	defer unlockDirty()

	unlock := r.lockDirtyFile(normRef, dir)
	defer unlock()

	n, err := r.loadDirtyNode(normRef, dir)
	if err != nil {
		return err
	}
	if n == nil {
		baseTree := ""
		if c, cerr := r.commitOf(normRef); cerr == nil {
			baseTree = c.Tree
		}
		items, cerr := r.cleanItemsAt(baseTree, dir)
		if cerr != nil {
			return cerr
		}
		n = startDirtyNode(items)
	}
	n.set[name] = dirtyEntry{Mode: mode, SHA: sha}
	delete(n.del, name)
	return r.saveDirtyNode(normRef, dir, n)
}

func (r *Repo) applyDirtyDelete(ref, path string) error {
	normRef := normalizeRef(ref)
	dir, name := splitDirName(path)

	unlockDirty := r.rlockDirty()
	defer unlockDirty()

	unlock := r.lockDirtyFile(normRef, dir)
	defer unlock()

	n, err := r.loadDirtyNode(normRef, dir)
	if err != nil {
		return err
	}
	if n == nil {
		baseTree := ""
		if c, cerr := r.commitOf(normRef); cerr == nil {
			baseTree = c.Tree
		}
		items, cerr := r.cleanItemsAt(baseTree, dir)
		if cerr != nil {
			return cerr
		}
		n = startDirtyNode(items)
	}
	delete(n.set, name)
	n.del[name] = true
	return r.saveDirtyNode(normRef, dir, n)
}

// Flush 等待当前排队的全部脏写入完成落盘。
func (r *Repo) Flush() error {
	return r.syncDirtyWriter()
}

// Pending 返回某分支（或全部分支）待提交的条数。
//
// 「条数」= 脏层里被改动过的名字总数（含墓碑）。它是判断
// 「有没有东西等着被折叠」的唯一依据——不再有第二份内存账本。
func (r *Repo) Pending(ref ...string) int {
	_ = r.syncDirtyWriter()
	unlockDirty := r.rlockDirty()
	defer unlockDirty()
	return r.pendingOnDisk(ref...)
}

// pendingOnDisk 在已持 dirtyMu 的前提下统计脏层条目数。
func (r *Repo) pendingOnDisk(ref ...string) int {

	count := func(name string) int {
		dirs, err := r.listDirtyDirs(name)
		if err != nil {
			return 0
		}
		total := 0
		for _, d := range dirs {
			n, err := r.loadDirtyNode(name, d)
			if err != nil || n == nil {
				continue
			}
			total += n.count()
		}
		return total
	}

	if len(ref) > 0 && ref[0] != "" {
		return count(normalizeRef(ref[0]))
	}
	refs, err := r.dirtyRefs()
	if err != nil {
		return 0
	}
	total := 0
	for _, name := range refs {
		total += count(name)
	}
	return total
}

// Commit 把脏层折叠成 Git 对象并推进 ref。
//
// 全程持全局写锁：折叠必须看到一份不动的脏层，否则
// 「读一半的脏节点 + 改了一半的脏节点」会写出一个从未存在过的树。
//
// 顺序（任一步崩溃都可重试，因为对象是内容寻址的）：
//
//	① 每个脏目录 → 一个 tree 对象
//	② 自底向上把新 tree 挂进祖先目录
//	③ 写 commit 对象
//	④ 原子替换 ref          ← 唯一的提交点
//	⑤ 删脏层
//
// ④ 之后 ⑤ 之前崩溃：ref 已前进但脏文件还在。下次 Commit 会发现
// 脏层描述的内容已经在 ref 里（折叠是幂等的），于是直接清掉它。
func (r *Repo) Commit(ref, message string) (string, error) {
	normRef := normalizeRef(ref)
	if err := r.syncDirtyWriter(); err != nil {
		return "", err
	}

	release, err := r.acquireCommitToken(normRef)
	if err != nil {
		return "", err
	}
	defer release()

	unlockDirty := r.lockDirty()
	defer unlockDirty()

	dirs, err := r.listDirtyDirs(normRef)
	if err != nil {
		return "", err
	}
	if len(dirs) == 0 {
		return "", nil
	}

	oldCommit, err := r.Resolve(normRef)
	if err != nil {
		return "", err
	}
	baseTree := ""
	if oldCommit != "" {
		old, cerr := r.commitOf(normRef)
		if cerr != nil {
			return "", cerr
		}
		baseTree = old.Tree
	}

	// 折叠：脏目录 + 它们的全部祖先，按深度降序（先算最深的子目录，
	// 再算它的父目录），逐层把新 tree SHA 挂上去。
	//
	// 为什么必须包含祖先：写 a/b/c.md 只会让 a/b 变脏，但 a 和根
	// 的 tree 指针也得跟着换，否则新内容在历史上隐形。
	needed := map[string]bool{}
	for _, dir := range dirs {
		needed[dir] = true
		for d := dir; d != ""; {
			parent, _ := splitDirName(d)
			if parent == d {
				break
			}
			needed[parent] = true
			d = parent
		}
	}
	toFold := make([]string, 0, len(needed))
	for d := range needed {
		toFold = append(toFold, d)
	}
	sortDirtyDirsDeepFirst(toFold)

	patched := map[string]string{}
	for _, dir := range toFold {
		items, cerr := r.cleanItemsAt(baseTree, dir)
		if cerr != nil {
			return "", cerr
		}
		n, lerr := r.loadDirtyNode(normRef, dir)
		if lerr != nil {
			return "", lerr
		}
		items = applyDirty(items, n)
		// 子目录已经算出新 SHA，最后叠上：新内容不能被子目录里的
		// 旧指针盖回去。同名冲突（文件/目录重名）时保留已有项。
		items = patchChildDirs(items, dir, patched)

		sha, werr := r.writeTreeItems(items)
		if werr != nil {
			return "", werr
		}
		patched[dir] = sha
	}
	rootSHA, ok := patched[""]
	if !ok {
		rootSHA = baseTree
	}

	// ③ 写 commit
	var parents []string
	if oldCommit != "" {
		parents = []string{oldCommit}
	}
	commitSHA, err := r.writeCommit(rootSHA, parents, message, time.Now())
	if err != nil {
		return "", err
	}

	// ④ 唯一的提交点
	if err := r.updateRef("refs/heads/"+normRef, commitSHA); err != nil {
		return "", fmt.Errorf("更新 ref: %w", err)
	}

	// ⑤ 清脏层。失败不算提交失败：内容已经在 ref 里了，
	//    残留的脏文件会在下一次 Commit 时被识别为「已折叠」并清掉。
	_ = r.clearDirtyRef(normRef)
	return commitSHA, nil
}

// sortDirtyDirsDeepFirst 按深度降序排（深的在前）。
func sortDirtyDirsDeepFirst(dirs []string) {
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if di != dj {
			return di > dj
		}
		return dirs[i] > dirs[j]
	})
}

// patchChildDirs 把已经被折叠过的直接子目录替换成新的 tree SHA；
// 折叠过程中新建出来的子目录原本不在这一层里，补上。
func patchChildDirs(items []treeItem, dir string, patched map[string]string) []treeItem {
	byName := make(map[string]treeItem, len(items))
	for _, it := range items {
		byName[it.name] = it
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	for child, sha := range patched {
		if !strings.HasPrefix(child, prefix) || child == dir {
			continue
		}
		rest := strings.TrimPrefix(child, prefix)
		if strings.Contains(rest, "/") {
			continue
		}
		if old, ok := byName[rest]; ok && old.mode != "40000" {
			continue
		}
		byName[rest] = treeItem{mode: "40000", name: rest, sha: sha}
	}
	out := make([]treeItem, 0, len(byName))
	for _, it := range byName {
		out = append(out, it)
	}
	return out
}

// writeTreeItems 把一组条目写成一个 tree 对象。
func (r *Repo) writeTreeItems(items []treeItem) (string, error) {
	if len(items) == 0 {
		return r.writeObject("tree", nil)
	}
	return r.writeObject("tree", serializeTree(items))
}

// rebuildAncestors 从 patched 出发自底向上重建祖先目录，返回新的根 tree。
//
// 只处理「被脏节点触及的目录」的祖先链，深度通常 2~4 层，
// 与树的规模无关。
func (r *Repo) rebuildAncestors(baseTree string, patched map[string]string) (string, error) {
	if sha, ok := patched[""]; ok {
		return sha, nil
	}
	// 找出所有需要重建的祖先目录（patched 里每个目录的父目录链）。
	pending := map[string]bool{}
	for dir := range patched {
		for dir != "" {
			dir, _ = splitDirName(dir)
			pending[dir] = true
		}
	}
	dirs := make([]string, 0, len(pending))
	for d := range pending {
		dirs = append(dirs, d)
	}
	sortDirtyDirsDeepFirst(dirs)

	for _, dir := range dirs {
		items, err := r.cleanItemsAt(baseTree, dir)
		if err != nil {
			return "", err
		}
		items = patchChildDirs(items, dir, patched)
		sha, err := r.writeTreeItems(items)
		if err != nil {
			return "", err
		}
		patched[dir] = sha
	}
	if sha, ok := patched[""]; ok {
		return sha, nil
	}
	return baseTree, nil
}

// objectTime 取一个对象文件的修改时间（Unix 秒），取不到返回 0。
func (r *Repo) objectTime(sha string) int64 {
	if err := validSHA(sha); err != nil {
		return 0
	}
	info, err := r.FS().Stat(r.objectPath(sha))
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

// ── 历史 ──────────────────────────────────────────────────────

// CommitInfo 是一条提交摘要。
type CommitInfo struct {
	SHA     string
	When    time.Time
	Message string
}

// Log 返回最近 n 条提交（n<=0 表示全部）。
func (r *Repo) Log(ref string, n int) ([]CommitInfo, error) {
	sha, err := r.Resolve(ref)
	if err != nil || sha == "" {
		return nil, err
	}
	var out []CommitInfo
	for sha != "" && (n <= 0 || len(out) < n) {
		typ, body, err := r.readObject(sha)
		if err != nil {
			break
		}
		if typ != "commit" {
			break
		}
		c := parseCommit(body)
		out = append(out, CommitInfo{SHA: sha, When: c.When, Message: strings.TrimSpace(c.Message)})
		if len(c.Parents) == 0 {
			break
		}
		sha = c.Parents[0]
	}
	return out, nil
}

// fileTimes 返回每个路径最后一次被改动的提交时间。
func (r *Repo) fileTimes(ref string) (map[string]int64, error) {
	commits, err := r.Log(ref, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	seen := map[string]bool{}
	// 从最新往旧走：第一次见到的路径就是它最后一次被改动
	for _, ci := range commits {
		typ, body, err := r.readObject(ci.SHA)
		if err != nil || typ != "commit" {
			continue
		}
		c := parseCommit(body)
		if c.Tree == "" {
			continue
		}
		flat := map[string]Entry{}
		if err := r.loadTree(c.Tree, "", flat); err != nil {
			continue
		}
		for p := range flat {
			if !seen[p] {
				seen[p] = true
				out[p] = ci.When.Unix()
			}
		}
	}
	return out, nil
}

// Fork 从 from 分支复制出 to 分支（零字节：只写一个 ref）。
func (r *Repo) Fork(from, to string) error {
	normTo := normalizeRef(to)
	release, err := r.acquireCommitToken(normTo)
	if err != nil {
		return err
	}
	defer release()

	sha, err := r.Resolve(from)
	if err != nil {
		return err
	}
	if sha == "" {
		return fmt.Errorf("分支 %s 不存在", from)
	}
	// 重置/分叉时把目标分支的脏层清掉：重置就是放弃所有未提交状态，
	// 继承源分支的纯净快照。不清掉的话，之前没提交的写入会叠加在
	// 新快照上，看起来像「没重置干净」。
	unlockDirty := r.lockDirty()
	_ = r.clearDirtyRef(normTo)
	unlockDirty()
	return r.updateRef("refs/heads/"+normTo, sha)
}

// BlobHash 算出这段内容的 blob SHA-1，不写入对象库。
func (r *Repo) BlobHash(data []byte) (string, error) {
	h := sha1OfBlob(data)
	return h, nil
}

// ── 校验 ──────────────────────────────────────────────────────

func validRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("ref 不能为空")
	}
	if strings.Contains(ref, "..") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return fmt.Errorf("非法 ref %q", ref)
	}
	return nil
}

func validPath(path string) error {
	if path == "" {
		return fmt.Errorf("路径不能为空")
	}
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return fmt.Errorf("路径不能以 / 开头或结尾：%q", path)
	}
	// Windows 上 : * ? " < > | 会被当成非法字符，而我们希望同一份记忆
	// 在三个平台上路径一致——所以一律拒绝。
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("路径不能含 NUL")
	}
	if strings.ContainsAny(path, `:*?"<>|\\`) {
		return fmt.Errorf("路径不能含 : * ? \" < > | \\ 等字符：%q", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			return fmt.Errorf("路径不能含空段：%q", path)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("路径不能含 . 或 .. 段：%q", path)
		}
	}
	return nil
}

// normalizeRef 把 "refs/heads/x" 归一成 "x"。
func normalizeRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// ── 脏层恢复 ──────────────────────────────────────────────────
//
// 脏层本身就是持久层：每个被改过的目录一个文件，改了就原子替换。
// 于是「恢复」不需要任何日志格式——扫一遍 dirty/<branch>/ 就够了，
// 每个文件的内容就是那个目录的当前覆盖。
//
// 这与旧版 WAL 的差别值得记下来：WAL 记的是「发生过什么」（事件流），
// 恢复要重放、要校验 CRC、要在坏行处截断。脏文件记的是「现在是什么」
// （状态快照），恢复只是读，没有重放，也没有「半行毒化后续记录」
// 这类问题——一个坏文件最多让那个目录退回干净层。

// RecoveryResult 报告一次启动恢复的概况。
type RecoveryResult struct {
	// Refs 是仍有未提交内容的逻辑分支名。
	Refs []string
	// Dirs 是恢复出来的脏目录数。
	Dirs int
	// Entries 是脏层里被改动过的名字总数（含墓碑）。
	Entries int
}

// Recover 扫描脏层，报告还有哪些分支有待折叠的改动。
//
// 它不改任何东西：脏文件已经是权威状态，重启后直接生效。
// 唯一做的是清理「孤儿脏文件」——那些所属分支连 ref 都不存在的目录
// （比如分支被 archive 掉了），留着只会让 Pending 永远不为零。
func (r *Repo) Recover() (RecoveryResult, error) {
	var res RecoveryResult

	refs, err := r.dirtyRefs()
	if err != nil {
		return res, err
	}

	unlockDirty := r.lockDirty()
	defer unlockDirty()

	for _, name := range refs {
		dirs, derr := r.listDirtyDirs(name)
		if derr != nil {
			continue
		}
		if len(dirs) == 0 {
			continue
		}
		// 分支已不存在时保留脏层：它可能是有意的「暂存分支」，
		// 仅凭没有 ref 不足以判定数据是孤儿。archive/reset 会显式清理。
		entries := 0
		for _, d := range dirs {
			n, lerr := r.loadDirtyNode(name, d)
			if lerr != nil || n == nil {
				continue
			}
			entries += n.count()
		}
		if entries == 0 {
			_ = r.clearDirtyRef(name)
			continue
		}
		res.Refs = append(res.Refs, name)
		res.Dirs += len(dirs)
		res.Entries += entries
	}
	return res, nil
}

// Delete 把删除动作投递进脏写入队列。
func (r *Repo) Delete(ref, path string) error {
	if err := validPath(path); err != nil {
		return err
	}
	r.writerMu.Lock()
	if r.writerDead {
		r.writerMu.Unlock()
		return fmt.Errorf("仓库 %s 已关闭，拒绝删除：%s", filepath.Base(r.Path), path)
	}
	r.initDirtyWriter()
	if len(r.writerCh) >= dirtyQueueCapacity {
		r.writerMu.Unlock()
		return fmt.Errorf("脏写入队列已满，拒绝删除：%s", path)
	}
	select {
	case r.writerCh <- dirtyTask{op: 'D', ref: ref, path: path}:
		r.writerMu.Unlock()
		return nil
	default:
		r.writerMu.Unlock()
		return fmt.Errorf("脏写入队列已满，拒绝删除：%s", path)
	}
}

// Deleted 返回某分支待提交的删除路径。
func (r *Repo) Deleted(ref string) []string {
	if err := r.syncDirtyWriter(); err != nil {
		return nil
	}
	unlockDirty := r.rlockDirty()
	defer unlockDirty()
	normRef := normalizeRef(ref)
	dirs, err := r.listDirtyDirs(normRef)
	if err != nil {
		return nil
	}
	var out []string
	for _, dir := range dirs {
		n, err := r.loadDirtyNode(normRef, dir)
		if err != nil || n == nil {
			continue
		}
		for name := range n.del {
			path := name
			if dir != "" {
				path = dir + "/" + name
			}
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// ── tag ───────────────────────────────────────────────────────

// Tags 列出所有 tag，返回 name -> commit SHA。
func (r *Repo) Tags() (map[string]string, error) {
	out := map[string]string{}
	root := filepath.Join(r.Path, "refs", "tags")
	err := r.FS().WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		b, rerr := r.FS().ReadFile(p)
		if rerr != nil {
			return nil
		}
		sha := strings.TrimSpace(string(b))
		if strings.HasPrefix(sha, "ref: ") {
			sha, _ = r.Resolve(strings.TrimSpace(strings.TrimPrefix(sha, "ref: ")))
		}
		if validSHA(sha) == nil {
			out[filepath.ToSlash(rel)] = sha
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// packed-refs 里的 tag
	if b, rerr := r.FS().ReadFile(filepath.Join(r.Path, "packed-refs")); rerr == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
				continue
			}
			sha, name, ok := strings.Cut(line, " ")
			if ok && strings.HasPrefix(name, "refs/tags/") && validSHA(sha) == nil {
				if _, dup := out[strings.TrimPrefix(name, "refs/tags/")]; !dup {
					out[strings.TrimPrefix(name, "refs/tags/")] = sha
				}
			}
		}
	}
	return out, nil
}

// Tag 给某个分支的当前状态打一个轻量 tag。
//
// 轻量 tag 就是一个指向 commit 的 ref，不带 tagger 信息。
// 记忆库不需要 annotated tag 的那套元数据——要记「为什么打这个标」
// 就写在切片里，那才是这套系统的正文。
func (r *Repo) Tag(name, ref string) error {
	if name == "" {
		return fmt.Errorf("tag 名不能为空")
	}
	if strings.ContainsAny(name, " ~^:?*[\\") || strings.Contains(name, "..") {
		return fmt.Errorf("非法 tag 名 %q", name)
	}
	sha, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	if sha == "" {
		return fmt.Errorf("分支 %q 不存在，无法打 tag", ref)
	}
	return r.updateRef("refs/tags/"+name, sha)
}

// Untag 删除一个 tag（挪到 refs/tags-removed/，不真删）。
//
// 为什么要留档：tag 往往标记「重构前的那个状态」。删掉它就等于
// 扔掉唯一的入口，而这个操作最容易在事后才被发现是错的。
func (r *Repo) Untag(name string) error {
	src := filepath.Join(r.Path, "refs", "tags", filepath.FromSlash(name))
	b, err := r.FS().ReadFile(src)
	if err != nil {
		return fmt.Errorf("tag %q 不存在", name)
	}
	dstDir := filepath.Join(r.Path, "refs", "tags-removed")
	if err := r.FS().MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(dstDir, strings.ReplaceAll(name, "/", "_")+"."+stamp)
	if err := r.FS().WriteFile(dst, b, 0o644); err != nil {
		return err
	}
	return r.FS().Remove(src)
}

// ── 跨分支操作 ─────────────────────────────────────────────────

// Copy 把一个路径的内容从 from 分支复制到 to 分支的同名路径。
func (r *Repo) Copy(from, to, path string) error {
	return r.CopyAs(from, path, to, path)
}

// CopyAs 把 srcPath 的内容复制到 dstPath。
//
// 跨分支搬运时源路径和目标路径经常不同（比如把带日期的切片挪进
// quarantine/xxx.md），所以必须分开传。只传一个 path 的版本在
// 「改了目标路径」时会静默用源路径落盘——不报错，只是放错地方。
func (r *Repo) CopyAs(from, srcPath, to, dstPath string) error {
	data, err := r.Read(from, srcPath)
	if err != nil {
		return err
	}
	mode := ModeContent
	if entries, err := r.List(from); err == nil {
		for _, e := range entries {
			if e.Path == srcPath {
				mode = e.Mode
				break
			}
		}
	}
	return r.Write(to, dstPath, data, mode)
}

// Move 把路径从 from 分支移到 to 分支：在 from 打墓碑，在 to 写入。
//
// 两个分支最终会被各自的 maintain 周期提交成两个独立 commit，
// 所以中间有一个瞬间是「两边都有」。这不是缺陷——真要原子跨分支
// 移动，就得放弃「一个分支一个 ref」的 Git 模型。
func (r *Repo) Move(from, to, path string) error {
	return r.MoveAs(from, path, to, path)
}

// MoveAs 把 srcPath 移到 dstPath（可以在不同分支）。
func (r *Repo) MoveAs(from, srcPath, to, dstPath string) error {
	if err := r.CopyAs(from, srcPath, to, dstPath); err != nil {
		return err
	}
	return r.Delete(from, srcPath)
}

// ── 分支比对 ───────────────────────────────────────────────────

// Change 是一条差异。
type Change struct {
	Path string
	// Kind: "added" / "modified" / "deleted"
	Kind string
}

// Diff 比对两个分支的内容，返回相对 from 而言 to 有哪些变化。
//
// 比对的是**路径 + blob SHA**，不是文件内容。内容寻址让这件事变成
// 纯哈希比较：同一个 SHA 必然同内容，不需要读任何 blob。
//
// from 为空串表示「和空树比」，即列出 to 的全部文件——
// 审计 Agent 第一次扫某个脏流分支时就是这个用法。
func (r *Repo) Diff(from, to string) ([]Change, error) {
	oldEntries := map[string]Entry{}
	if from != "" {
		list, err := r.List(from)
		if err != nil {
			return nil, fmt.Errorf("读 %s: %w", from, err)
		}
		for _, e := range list {
			oldEntries[e.Path] = e
		}
	}

	newList, err := r.List(to)
	if err != nil {
		return nil, fmt.Errorf("读 %s: %w", to, err)
	}

	var out []Change
	newSeen := map[string]bool{}
	for _, e := range newList {
		newSeen[e.Path] = true
		old, existed := oldEntries[e.Path]
		switch {
		case !existed:
			out = append(out, Change{Path: e.Path, Kind: "added"})
		case old.Hash != e.Hash:
			out = append(out, Change{Path: e.Path, Kind: "modified"})
		}
	}
	for p := range oldEntries {
		if !newSeen[p] {
			out = append(out, Change{Path: p, Kind: "deleted"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Exists 判断某分支下某个路径是否存在。
func (r *Repo) Exists(ref, path string) bool {
	_, err := r.Read(ref, path)
	return err == nil
}

// ArchiveRef 把一个分支的 ref 移到 refs/archived/ 下。
//
// 按「不真删」的规矩：分支从 refs/heads/ 消失（不再出现在列表里），
// 但内容完好地留在 refs/archived/，随时可以移回来。
func (r *Repo) ArchiveRef(branch string) error {
	name := normalizeRef(branch)
	sha, err := r.Resolve(name)
	if err != nil || sha == "" {
		return fmt.Errorf("分支 %q 不存在", branch)
	}
	dstDir := filepath.Join(r.Path, "refs", "archived")
	if err := r.FS().MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	if err := r.FS().WriteFile(filepath.Join(dstDir, name), []byte(sha+"\n"), 0o644); err != nil {
		return err
	}
	return r.FS().Remove(filepath.Join(r.Path, "refs", "heads", name))
}

// Branches 列出该仓库所有的分支名（refs/heads/ 下的短名）。
// Branches 列出本地分支（短名）。
//
// 依赖 Refs 合并 packed-refs：git clone --bare 出来的仓库把分支全放在
// packed-refs 里，refs/heads 是空目录。早先直接走目录，于是这样的仓库
// 被判定为「没有分支」，索引为 0 块，而服务照常报 ok:true。
func (r *Repo) Branches() []string {
	refs, err := r.Refs("refs/heads/")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, name := range refs {
		out = append(out, strings.TrimPrefix(name, "refs/heads/"))
	}
	return out
}

// PendingRefs 列出脏层里有待折叠改动的分支名。
// 先等写入队列排空，确保看到最新状态。
func (r *Repo) PendingRefs() []string {
	_ = r.syncDirtyWriter()
	unlockDirty := r.rlockDirty()
	defer unlockDirty()
	refs, _ := r.dirtyRefs()
	return refs
}
