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

	// dirtySyncPending 记录已写但未 fsync 的脏文件（见 SyncDirtyNow）。
	dirtySyncPending sync.Map
	// dirtyCache 是脏层节点的进程内缓存（ref\x00dir -> *dirtyNode）。
	//
	// 没有它，每次写入都要把整个脏文件读出来解析一遍算差量——
	// 实测 3000 条同目录写入，单条成本仍然从 0.24ms 涨到 1.15ms。
	// 有了它，写入是纯内存改一个 map + append 一行。
	dirtyCache sync.Map

	// fsBox / lockerBox 是懒初始化的原子槽。
	//
	// 原来用 mu 保护「为 nil 就赋值」——那是典型的「检查并置位」，
	// 靠人保证多条 return 路径配平锁。CompareAndSwap 把检查与置位
	// 合成一次原子操作，对错当场可判。
	fsBox     atomic.Pointer[fsHolder]
	lockerBox atomic.Pointer[lockerHolder]

	// writer 是单线程脏层写者。Write/Delete 只把任务投递进队列就返回；
	// Read/List/Commit 会排一个 barrier，保证此前已接受的任务先处理完。
	writerOnce sync.Once
	writerCh   chan dirtyTask
	// writerTok 是一槽 channel 令牌，串行化「入队 / 关闭 / barrier」这三种动作。
	// barrier 必须排在先前写入之后，这个顺序由令牌保证。
	//
	// 为什么不用 Mutex：下面三个函数每一个都有 3~5 条 return 路径，
	// 每条都必须记得 unlock。令牌配 defer 只有一条路径需要写对。
	writerTokPtr atomic.Pointer[chan struct{}]
	writerSize atomic.Int64
	writerErr  error         // worker 累积的异步错误，下一次 barrier 返回并清空
	writerDown chan struct{} // worker 退出后关闭，Close 等它
	writerDead bool          // 队列已关，不再接受新投递

}

type fsHolder struct{ fs FS }
type lockerHolder struct{ locker Locker }

// writerTok 返回写者令牌（懒创建）。零值 Repo 也能用。
func (r *Repo) writerTok() chan struct{} {
	if p := r.writerTokPtr.Load(); p != nil {
		return *p
	}
	ch := make(chan struct{}, 1)
	if r.writerTokPtr.CompareAndSwap(nil, &ch) {
		return ch
	}
	return *r.writerTokPtr.Load()
}

// lockWriter 取写者令牌，返回释放函数。超时后返回错误而不是无限等待。
func (r *Repo) lockWriter(what string) (func(), error) {
	ch := r.writerTok()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	default:
	}
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-t.C:
		return nil, fmt.Errorf("%s: 等写者令牌超时（30s），有写者卡在落盘上", what)
	}
}

// dirtySyncPending 记录「已写但还没 fsync」的脏文件路径。
//
// 为什么不每次写都 fsync：绝大多数崩溃是进程崩溃而不是断电，
// 页缓存还在，数据不会丢。真正需要防的是断电/内核崩，而那种情况
// 攒几秒批量刷一次就够了。每次写都 fsync 会把单条写入从
// 0.5ms 拖到几毫秒，而收益只在「恰好断电」这个窄窗口里。
//
// 实现用 sync.Map：路径之间互相独立，没有跨 key 不变量。
func (r *Repo) markDirtyForSync(path string) {
	r.dirtySyncPending.Store(path, struct{}{})
}

// SyncDirtyNow 把所有待刷盘的脏文件 fsync 落盘，并清空待刷集合。
//
// 调用时机：后台维护环每几秒一次，以及 Close 之前。
// 目录项也要同步——新建的脏文件只有目录项落盘后才真的存在。
func (r *Repo) SyncDirtyNow() error {
	var paths []string
	r.dirtySyncPending.Range(func(k, _ any) bool {
		paths = append(paths, k.(string))
		return true
	})
	if len(paths) == 0 {
		return nil
	}
	var firstErr error
	syncedDirs := map[string]struct{}{}
	for _, p := range paths {
		if err := syncFile(p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		r.dirtySyncPending.Delete(p)
		syncedDirs[filepath.Dir(p)] = struct{}{}
	}
	for d := range syncedDirs {
		if err := syncDir(d); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// syncFile 对单个已存在的文件做 fsync。
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return f.Sync()
}

// FS 返回当前仓库使用的存储抽象。
func (r *Repo) FS() FS {
	if h := r.fsBox.Load(); h != nil {
		return h.fs
	}
	h := &fsHolder{fs: DefaultFS()}
	if r.fsBox.CompareAndSwap(nil, h) {
		return h.fs
	}
	return r.fsBox.Load().fs
}

// Locker 返回当前仓库使用的锁管理器。
func (r *Repo) Locker() Locker {
	if h := r.lockerBox.Load(); h != nil {
		return h.locker
	}
	h := &lockerHolder{locker: newLocalLocker()}
	if r.lockerBox.CompareAndSwap(nil, h) {
		return h.locker
	}
	return r.lockerBox.Load().locker
}

func (r *Repo) rlockDirty() func() { return r.Locker().RLockDirty() }
func (r *Repo) lockDirty() func()  { return r.Locker().LockDirty() }
func (r *Repo) lockNode(ref, dir string) (func(), error) {
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
		return nil, fmt.Errorf("repository %s: %w", abs, err)
	}
	for _, want := range []string{"HEAD", "objects"} {
		if _, err := storage.Stat(filepath.Join(abs, want)); err != nil {
			return nil, fmt.Errorf("%s is not a bare repository (missing %s)", abs, want)
		}
	}
	r := &Repo{Path: abs}
	if storage != nil {
		r.fsBox.Store(&fsHolder{fs: storage})
	}
	if locker != nil {
		r.lockerBox.Store(&lockerHolder{locker: locker})
	}
	// 脏层本身就是持久层，重启后直接生效；这里只清掉孤儿脏层
	// （所属分支已不存在）并统计还有多少改动等着折叠。
	// 失败不影响打开：脏层文件读不出来最多让那个目录退回干净层。
	_, _ = r.Recover()
	return r, nil
}

// Init 初始化一个裸仓库。全部用标准库，不调 git。
// DefaultBranch 是仓库的默认分支名。
//
// 它只在「调用方没有指定」时兜底：新仓库的 HEAD、挂载点补全、
// git 协议广告里的 symref。它不是身份，也不该被当成权限判据。
const DefaultBranch = "zero"

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
		if err := storage.WriteFile(head, []byte("ref: refs/heads/"+DefaultBranch+"\n"), 0o644); err != nil {
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
			if isNotExist(err) {
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
	// 越界名字不能悄悄变成「不存在」（那会把一次穿越尝试伪装成
	// 「这个分支还没建」），但合法名字的「不存在」是正常返回值。
	if err := ValidateBranch(strings.TrimPrefix(ref, "refs/")); err != nil {
		return "", err
	}
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
//
// 这里是所有「推进分支指针」的唯一出口，所以 ref 名校验也必须在这里：
// ref 名会直接变成 refs/ 下的文件路径，放任 "../../.." 进来
// 就能把文件写到仓库外面去（已验证：Fork 可以把 ref 写到 data 目录旁边）。
// 调用方（Commit / Fork / Tag）传的是拼好的完整 ref，逐个校验
// 会漏掉一个——入口统一校验才没有漏网的。
func (r *Repo) updateRef(ref, sha string) error {
	if err := validFullRef(ref); err != nil {
		return err
	}
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
	sha := ref
	// 如果不是 40 位 hex SHA，才当成 ref 解析。
	if validSHA(ref) != nil {
		resolved, err := r.Resolve(ref)
		if err != nil || resolved == "" {
			return commitObj{}, err
		}
		sha = resolved
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return commitObj{}, err
	}
	if typ != "commit" {
		return commitObj{}, fmt.Errorf("%s is not a commit but %s", sha, typ)
	}
	return parseCommit(body), nil
}

// List 列出某个分支下的全部条目（干净树 + 脏层覆盖）。
func (r *Repo) List(ref string) ([]Entry, error) {
	if err := r.syncDirtyWriter(); err != nil {
		return nil, fmt.Errorf("process dirty write queue: %w", err)
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
		return nil, fmt.Errorf("process dirty write queue: %w", err)
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
			return nil, fmt.Errorf("%s does not exist (deleted, not yet committed)", path)
		}
		if e, ok := n.set[name]; ok && e.Mode != "40000" {
			unlockDirty()
			typ, body, err := r.readObject(e.SHA)
			if err != nil {
				return nil, err
			}
			if typ != "blob" {
				return nil, fmt.Errorf("%s is not a file but %s", path, typ)
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
		return nil, fmt.Errorf("%s is still empty", ref)
	}
	sha, err := r.findInTree(c.Tree, "", path)
	if err != nil {
		return nil, err
	}
	if sha == "" {
		return nil, fmt.Errorf("%s does not exist", path)
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return nil, err
	}
	if typ != "blob" {
		return nil, fmt.Errorf("%s is not a file but %s", path, typ)
	}
	return body, nil
}

// Stat 取单个路径的条目（路径、mode、大小、blob hash），不遍历整棵树。
//
// 为什么需要它：`List(ref)` 会展开整条分支（main.chat 有 5233 个路径），
// 而 write / delete 只需要知道「这一个文件现在长什么样」。
// 一次 write 为此走一遍全分支，实测 19 秒——规模越大越慢。
//
// findInTree 的复杂度是 O(路径深度)，与分支大小无关，这才是这里要的。
// 找不到时返回 (Entry{}, false, nil)：文件不存在不是错误。
func (r *Repo) Stat(ref, path string) (Entry, bool, error) {
	if err := r.syncDirtyWriter(); err != nil {
		return Entry{}, false, fmt.Errorf("process dirty write queue: %w", err)
	}
	normRef := normalizeRef(ref)
	dir, name := splitDirName(path)

	e := Entry{Path: strings.Trim(path, "/")}

	// ① 脏层优先：刚写入还没提交的文件也必须能 stat 到。
	unlockDirty := r.rlockDirty()
	n, err := r.loadDirtyNode(normRef, dir)
	if err != nil {
		unlockDirty()
		return Entry{}, false, err
	}
	if n != nil {
		if gone := n.del[name]; gone {
			unlockDirty()
			return Entry{}, false, nil
		}
		if it, ok := n.set[name]; ok && it.Mode != "40000" {
			unlockDirty()
			e.Mode = it.Mode
			e.Hash = it.SHA
			if typ, body, rerr := r.readObject(it.SHA); rerr == nil && typ == "blob" {
				e.Size = int64(len(body))
			}
			e.Time = r.objectTime(it.SHA)
			return e, true, nil
		}
	}
	unlockDirty()

	// ② 干净层
	c, err := r.commitOf(normRef)
	if err != nil {
		return Entry{}, false, err
	}
	if c.Tree == "" {
		return Entry{}, false, nil
	}
	sha, err := r.findInTree(c.Tree, "", e.Path)
	if err != nil {
		return Entry{}, false, err
	}
	if sha == "" {
		return Entry{}, false, nil
	}
	e.Hash = sha
	if typ, body, rerr := r.readObject(sha); rerr == nil && typ == "blob" {
		e.Size = int64(len(body))
	}
	e.Time = r.objectTime(sha)
	e.Mode = ModeContent
	return e, true, nil
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
		return "", fmt.Errorf("object %s is not a tree", treeSHA)
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
				return "", fmt.Errorf("%s is a directory", want)
			}
			return it.sha, nil
		}
		if it.mode != "40000" {
			return "", fmt.Errorf("%s is not a directory", head)
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

// Barrier 等待脏写入队列排空：返回时，此前投递的写入都已经落进脏层。
//
// 为什么需要它：Write 只承诺「已投递」（见其注释），而调用方往往要在
// 回包里写「ok」。只投递就回 ok 的后果实测过——put 回了 ok、路径也对，
// 但文件在磁盘和 git 里都不存在，commit 还说 "nothing pending"。
// 一次静默丢写比一次报错贵得多：报错会被重试，丢写没人知道。
func (r *Repo) Barrier() error {
	return r.syncDirtyWriter()
}

func (r *Repo) syncDirtyWriter() error {
	release, err := r.lockWriter("barrier")
	if err != nil {
		return err
	}
	if r.writerDead {
		down := r.writerDown
		release()
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
	release()
	return <-ch
}

// Close 停止单线程写者：拒绝新投递、排空已接受的队列、等待落盘完成。
//
// 为什么不复用 commit 的分支令牌：Close 是全仓库生命周期事件，
// 与某条分支的提交排队无关；它要做的只是「队列关闸、存量刷完、
// goroutine 退出」。幂等：重复调用与「从未写过」都安全。
func (r *Repo) Close() error {
	release, lerr := r.lockWriter("close")
	if lerr != nil {
		return lerr
	}
	if r.writerDead {
		down := r.writerDown
		release()
		if down != nil {
			<-down
		}
		return nil
	}
	r.writerDead = true
	if r.writerCh == nil {
		release()
		return nil
	}
	down := r.writerDown
	close(r.writerCh)
	release()
	<-down
	// 关停前把待刷盘的脏文件落盘：正常停机不该丢掉最后几条写入。
	if err := r.SyncDirtyNow(); err != nil && r.writerErr == nil {
		r.writerErr = err
	}
	// worker 已退出，不会再改 writerErr；返回关闭前尚未被 barrier
	// 观察到的最后一个异步写盘错误。
	return r.writerErr
}

// Write 把内容投递进脏写入队列。
//
// 语义：只承诺「已投递」。不等待落盘，不等待提交，返回即表示
// 已进入单写者队列。若队列已满则报错拒绝（调用方知晓未被采纳）。
func (r *Repo) Write(ref, path string, data []byte, mode Mode) error {
	// 分支名在这里就要拦：脏层目录同样按它拼路径（dirty/<branch>/），
	// 越界写入不经过 ref 文件，updateRef 的校验管不到。
	if err := ValidateBranch(normalizeRef(ref)); err != nil {
		return err
	}
	if err := validPath(path); err != nil {
		return err
	}
	if mode == "" {
		mode = ModeContent
	}

	size := int64(len(data))
	cp := make([]byte, len(data))
	copy(cp, data)

	release, lerr := r.lockWriter("enqueue-write")
	if lerr != nil {
		return lerr
	}
	defer release()
	if r.writerDead {
		return fmt.Errorf("repository %s is closed; refusing new writes", filepath.Base(r.Path))
	}
	r.initDirtyWriter()
	queuedSize := r.writerSize.Load()
	if len(r.writerCh) >= dirtyQueueCapacity || queuedSize+size > dirtyQueueMaxBytes {
		return fmt.Errorf("dirty write queue is full (%d pending / %d bytes); refusing new writes, retry later", len(r.writerCh), queuedSize)
	}
	r.writerSize.Add(size)
	select {
	case r.writerCh <- dirtyTask{op: 'W', ref: ref, path: path, data: cp, mode: mode, size: size}:
		return nil
	default:
		r.writerSize.Add(-size)
		return fmt.Errorf("dirty write queue is full; refusing new writes, retry later")
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

	unlock, lerr := r.lockDirtyFile(normRef, dir)
	if lerr != nil {
		return lerr
	}
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

	unlock, lerr := r.lockDirtyFile(normRef, dir)
	if lerr != nil {
		return lerr
	}
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
	// 折叠要读 dirty/<branch>/ 并推进 refs/heads/<branch>，两处都拼路径。
	if err := ValidateBranch(normRef); err != nil {
		return "", err
	}
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

	// 折叠结果与当前树一模一样时，不要写空提交。
	//
	// 这个分支不是理论情况：④ 之后、⑤ 之前崩溃（或清理脏层失败）
	// 会留下「ref 已前进、脏文件还在」的形态。注释一直声称下次
	// Commit 会认出「已经折叠过」并直接清掉——但代码里没有这个判定，
	// 于是每次重试都多一个内容相同的 commit，脏层却永远清不掉。
	if rootSHA == baseTree && oldCommit != "" {
		_ = r.clearDirtyRef(normRef)
		return oldCommit, nil
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
		return "", fmt.Errorf("update ref: %w", err)
	}

	// ⑤ 清脏层。失败不算提交失败：内容已经在 ref 里了，
	//    残留的脏文件会在下一次 Commit 时被上面的「折叠结果与当前树
	//    相同」判定识别为已折叠并清掉。
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
//
// 踩过的坑（2026-11-13，hub 起不来，CPU 100% 十分钟、端口永不监听）：
// 旧实现是 `Log(ref, 0)` 取全部历史，然后**对每个 commit 都把整棵树
// 展开成 path map**——复杂度 O(commits × files)。在 inbox 这种
// 「一条消息一个 commit」的流水分支上是 2052 × 4520 ≈ 900 万次树项遍历，
// 而且每次都要从 pack 里解压对象。启动时它在 Reload 里跑，`net.Listen`
// 排在后面，症状就是「进程活着、内存涨、端口永远不开、日志一行没有」。
//
// 现在的做法：沿 first-parent 链走，用 diffTree 比较相邻两棵树。
// diffTree 对相同子树按 SHA 短路，所以每个 commit 的开销等于**它实际
// 改动的路径数量**而不是仓库总文件数；总复杂度 O(changes)。
// 一旦所有现存路径都拿到了时间就提前结束，不需要走完整段历史。
// FileTimes 是 fileTimes 的导出形式（服务端的按日锚点要用它）。
func (r *Repo) FileTimes(ref string) (map[string]int64, error) { return r.fileTimes(ref) }

func (r *Repo) fileTimes(ref string) (map[string]int64, error) {
	normRef := normalizeRef(ref)
	tipSHA, err := r.Resolve(normRef)
	if err != nil || tipSHA == "" {
		return map[string]int64{}, nil
	}
	tip, err := r.commitOf(tipSHA)
	if err != nil {
		return nil, err
	}
	if tip.Tree == "" {
		return map[string]int64{}, nil
	}

	// 只需要给「现在还存在」的路径定时间。已删除的历史路径对调用方没用，
	// 拿它们当提前结束条件会让循环退化成走完全部历史。
	wanted := map[string]bool{}
	if err := r.collectPaths(tip.Tree, "", wanted); err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return map[string]int64{}, nil
	}

	// 锚点：某个 commit 上「每个路径的时间已经是多少」。
	//
	// 纯 tag 只能截断指针，走到那里仍然不知道更早的历史——所以锚点
	// 必须带着数据。有了它，遍历只需要覆盖「锚点之后」那一段。
	// 实测 main.chat（1,506 commit / 3,042 文件）就这么从 8.29s 降到毫秒级。
	cp, _ := r.LatestDailyCheckpoint(normRef)
	out := map[string]int64{}
	if cp != nil && cp.Commit == tipSHA {
		// tip 就是锚点：零遍历。
		cp.pick(wanted, out)
		if len(out) >= len(wanted) {
			return out, nil
		}
	}

	// 防御性上限：仓库再长也不该让启动无限期卡住。
	// 走到上限时剩下的路径回退到零值（调用方会改用正文里的 at:/created:）。
	const maxCommits = 200000

	cur := tip
	curSHA := tipSHA
	for i := 0; i < maxCommits; i++ {
		// 命中锚点：把剩余路径的时间一次补齐，停止回溯。
		// 锚点里的时间对它记录的那个 commit 永久成立，所以可以放心用。
		if cp != nil && curSHA == cp.Commit {
			cp.pick(wanted, out)
			break
		}

		parentTree := ""
		var parent *commitObj
		if len(cur.Parents) > 0 {
			pc, perr := r.commitOf(cur.Parents[0])
			if perr != nil {
				return nil, perr
			}
			parent = &pc
			parentTree = pc.Tree
		}

		var changes []Change
		if err := r.diffTree(parentTree, cur.Tree, "", &changes); err != nil {
			return nil, err
		}
		for _, ch := range changes {
			if ch.Kind == "deleted" {
				continue
			}
			if !wanted[ch.Path] {
				continue
			}
			if _, ok := out[ch.Path]; ok {
				continue
			}
			out[ch.Path] = cur.When.Unix()
		}
		if len(out) >= len(wanted) || parent == nil {
			break
		}
		curSHA = cur.Parents[0]
		cur = *parent
	}

	// 这里**不写**锚点：fileTimes 是读路径（List 会调它），
	// 读路径里写对象会让人无法解释「为什么读一次就多出来几个对象」。
	// 锚点由写入侧按天打（见 Server.maybeWriteDailyCheckpoint）。
	return out, nil
}

// collectPaths 把一棵树里的全部文件路径收进 set，用于计算「还差哪些路径」。
func (r *Repo) collectPaths(sha, prefix string, out map[string]bool) error {
	if sha == "" {
		return nil
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return err
	}
	if typ != "tree" {
		return fmt.Errorf("object %s is not a tree but %s", sha, typ)
	}
	items, err := parseTree(body)
	if err != nil {
		return err
	}
	for _, it := range items {
		full := it.name
		if prefix != "" {
			full = prefix + "/" + it.name
		}
		if it.mode == "40000" {
			if err := r.collectPaths(it.sha, full, out); err != nil {
				return err
			}
			continue
		}
		out[full] = true
	}
	return nil
}

// Fork 从 from 分支复制出 to 分支（零字节：只写一个 ref）。
func (r *Repo) Fork(from, to string) error {
	// from 要读、to 要写，两个名字都会变成 refs/heads/ 下的文件路径。
	if err := ValidateBranch(normalizeRef(to)); err != nil {
		return fmt.Errorf("invalid target branch name: %w", err)
	}
	if err := ValidateBranch(normalizeRef(from)); err != nil {
		return fmt.Errorf("invalid source branch name: %w", err)
	}
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
		return fmt.Errorf("branch %s does not exist", from)
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

// validateSegments 是「名字变路径」的唯一判据。
//
// 分支名、tag 名、远端名、项目名、树内路径，最终都是仓库目录下的
// 路径片段。它们的规则本来是同一套，却被抄成了四份
// （validRef、validFullRef、validPath、auth 里的两个 Validate*），
// 于是每加一条限制都要记得改四处——而漏改一处的结果是
// 「某个入口能写、另一个入口不能写」，这种不一致比规则本身更危险。
//
// 只在一处实现之后，「什么名字是合法的」只有一种答案。
func validateSegments(kind, name string, allowSlash bool) error {
	if name == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%s must not contain NUL: %q", kind, name)
	}
	// ~ ^ 是 Git 的 revision 语法字符，放进去会让 ref 名与命令行解析冲突。
	// 空格与 : ? * [ \ 同时被 Git 与 Windows 拒绝。
	if strings.ContainsAny(name, " ~^:?*[\\") {
		return fmt.Errorf("%s must not contain space ~ ^ : ? * [ \\ and similar: %q", kind, name)
	}
	// Windows 保留字符：同一份记忆要在三个平台上路径一致，一律拒绝。
	if strings.ContainsAny(name, `"<>|`) {
		return fmt.Errorf("%s must not contain \" < > | and similar: %q", kind, name)
	}
	if strings.ContainsAny(name, "\n\r\t") {
		return fmt.Errorf("%s must not contain control characters: %q", kind, name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%s must not contain ..: %q", kind, name)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("%s must not start or end with /: %q", kind, name)
	}
	if !allowSlash && strings.Contains(name, "/") {
		return fmt.Errorf("%s must not contain a path separator: %q", kind, name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return fmt.Errorf("%s must not contain an empty segment: %q", kind, name)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("%s must not contain . or .. segments: %q", kind, name)
		}
		// 以 . 开头：Git 自己就禁止；而且 "....//...." 这类绕法的每一段
		// 都是「. 加尾巴」，不是字面 ".."，路径解析后却仍回到上级。
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("no segment of %s may start with .: %q", kind, name)
		}
		if strings.HasSuffix(seg, ".") {
			return fmt.Errorf("no segment of %s may end with .: %q", kind, name)
		}
	}
	return nil
}

// ValidateName 校验一个会变成文件名的名字。allowSlash 区分两类：
// 分支/tag/远端/项目名允许层级（feat/x、blackegg/me）；
// 用户名与凭证名不进目录树，一个斜杠就是越界。
func ValidateName(kind, name string, allowSlash bool) error {
	return validateSegments(kind, name, allowSlash)
}

// ValidateBranch 校验逻辑分支名（归一化后的短名，如 "zero"、"feat/x"）。
func ValidateBranch(ref string) error { return validateSegments("ref", ref, true) }

// ValidateRepoName 校验项目（裸仓库）名，允许多层如 "blackegg/me"。
func ValidateRepoName(name string) error { return validateSegments("project name", name, true) }

// ValidateCredName 校验用户名/凭证名：它们不进目录树，不允许斜杠。
func ValidateCredName(name string) error { return validateSegments("name", name, false) }

// validPath 校验树内路径。与 ValidateBranch 同源，只是错误措辞不同
// （路径与分支名对使用者是两件事）。
func validPath(path string) error {
	return validateSegments("path", path, true)
}

// validFullRef 校验一个完整 ref，并限制到本仓库使用的命名空间。
func validFullRef(ref string) error {
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("ref must start with refs/: %q", ref)
	}
	if err := ValidateBranch(strings.TrimPrefix(ref, "refs/")); err != nil {
		return err
	}
	for _, ns := range []string{"refs/heads/", "refs/tags/", "refs/remotes/", "refs/archived/"} {
		if strings.HasPrefix(ref, ns) {
			return nil
		}
	}
	return fmt.Errorf("refusing to write ref %q: only refs/heads, refs/tags, refs/remotes and refs/archived are allowed", ref)
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
	// 与 Write 同理：墓碑写在 dirty/<branch>/ 里。
	if err := ValidateBranch(normalizeRef(ref)); err != nil {
		return err
	}
	if err := validPath(path); err != nil {
		return err
	}
	release, lerr := r.lockWriter("enqueue-delete")
	if lerr != nil {
		return lerr
	}
	defer release()
	if r.writerDead {
		return fmt.Errorf("repository %s is closed; refusing delete: %s", filepath.Base(r.Path), path)
	}
	r.initDirtyWriter()
	if len(r.writerCh) >= dirtyQueueCapacity {
		return fmt.Errorf("dirty write queue is full; refusing delete: %s", path)
	}
	select {
	case r.writerCh <- dirtyTask{op: 'D', ref: ref, path: path}:
		return nil
	default:
		return fmt.Errorf("dirty write queue is full; refusing delete: %s", path)
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
			if isNotExist(err) {
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
// TagWithMessage 建一个 annotated tag，正文是调用方给的摘要。
//
// 与 Tag 的区别：Tag 是轻量 tag（一个 ref 文件指向 commit，存不下任何话）；
// 这个会在对象库里写一个 tag 对象，正文就是「这一批记忆讲了什么」。
//
// 为什么摘要要挂在 tag 上而不是单独存一份：tag 本身就在 git 里，
// 随 push 到远端，`git show <tag>` 直接能读。它天然是「里程碑 + 摘要」，
// 不需要再发明一个格式。
func (r *Repo) TagWithMessage(name, ref, message string) (string, error) {
	if err := ValidateBranch(name); err != nil {
		return "", fmt.Errorf("invalid tag name: %w", err)
	}
	sha, err := r.Resolve(normalizeRef(ref))
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "", fmt.Errorf("%s has no commit to tag", ref)
	}
	tagSHA, err := r.writeTagObject(name, sha, message, time.Now())
	if err != nil {
		return "", err
	}
	if err := r.updateRef("refs/tags/"+name, tagSHA); err != nil {
		return "", err
	}
	return tagSHA, nil
}

func (r *Repo) Tag(name, ref string) error {
	// updateRef 会校验拼好的 ref，但那里报的是「ref 非法」。
	// 这里先校验一次是为了让错误信息说清是「tag 名」。
	if err := ValidateBranch(name); err != nil {
		return fmt.Errorf("invalid tag name: %w", err)
	}
	sha, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	if sha == "" {
		return fmt.Errorf("branch %q does not exist; cannot tag", ref)
	}
	return r.updateRef("refs/tags/"+name, sha)
}

// Untag 删除一个 tag（挪到 refs/tags-removed/，不真删）。
//
// 为什么要留档：tag 往往标记「重构前的那个状态」。删掉它就等于
// 扔掉唯一的入口，而这个操作最容易在事后才被发现是错的。
func (r *Repo) Untag(name string) error {
	if err := ValidateBranch(name); err != nil {
		return fmt.Errorf("invalid tag name: %w", err)
	}
	src := filepath.Join(r.Path, "refs", "tags", filepath.FromSlash(name))
	b, err := r.FS().ReadFile(src)
	if err != nil {
		return fmt.Errorf("tag %q does not exist", name)
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
// 第一次扫某个脏流分支时就是这个用法。
func (r *Repo) Diff(from, to string) ([]Change, error) {
	// 两个参数可以是分支名，也可以是 40 位 commit SHA。
	//
	// 为什么要支持 SHA：启动时用快照恢复索引，需要比对「快照记录的
	// 那个 commit」与「现在的 head」——前者只能是一个 SHA，它已经不是
	// 任何分支的 head 了。早先直接传给 List(commitOf→Resolve)，
	// 而 Resolve 会去 refs/heads/<40位十六进制> 找一个不存在的名字，
	// 返回空 → 两边都当空树 → 差量恒为 0。
	// 症状是「快照加载成功、差量应用 0 条、新写的内容搜不到」。
	listAt := func(ref string) ([]Entry, error) {
		if len(ref) == 40 {
			if err := validSHA(ref); err == nil {
				return r.listAtCommit(ref)
			}
		}
		return r.List(ref)
	}

	oldEntries := map[string]Entry{}
	if from != "" {
		list, err := listAt(from)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", from, err)
		}
		for _, e := range list {
			oldEntries[e.Path] = e
		}
	}

	newList, err := listAt(to)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", to, err)
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
	if err := ValidateBranch(name); err != nil {
		return fmt.Errorf("invalid branch name: %w", err)
	}
	sha, err := r.Resolve(name)
	if err != nil || sha == "" {
		return fmt.Errorf("branch %q does not exist", branch)
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

// listAtCommit 列出一个具体 commit 的树（不进脏层）。
//
// 与 List 的区别：List 是「这条分支现在有什么」（含未折叠的脏层），
// 这里问的是「那个 commit 当时有什么」——历史快照，不该被脏层污染。
func (r *Repo) listAtCommit(sha string) ([]Entry, error) {
	if err := validSHA(sha); err != nil {
		return nil, err
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return nil, err
	}
	if typ != "commit" {
		return nil, fmt.Errorf("%s is not a commit but %s", sha, typ)
	}
	c := parseCommit(body)
	out := map[string]Entry{}
	if c.Tree != "" {
		if err := r.loadTree(c.Tree, "", out); err != nil {
			return nil, err
		}
	}
	list := make([]Entry, 0, len(out))
	for _, e := range out {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	return list, nil
}
