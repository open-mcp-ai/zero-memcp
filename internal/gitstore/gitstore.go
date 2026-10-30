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
// 唯一的代价：读 packfile 需要额外实现。所以 gc 走的是「不打包」策略
// （见 maintain），松散对象长期保留。松散对象多到影响性能时，
// 调用外部 git gc 是可选优化，不是正确性依赖。
package gitstore

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	mu sync.Mutex
	// pending 按分支隔离：ref -> path -> data。
	//
	// 为什么必须按分支隔离：同一仓库的不同分支可以并发写入，
	// 如果 key 只有 path，两个分支写同名文件就会后写覆盖先写——
	// 而那正是「chat 和 dev 在同一天记录」时的真实场景。
	pending map[string]map[string][]byte
	pmode   map[string]map[string]Mode
	// pdeleted 记墓碑：ref -> 路径集合。
	//
	// 删除必须和写入一样走「待提交区 → 提交」这条路，不能直接改树。
	// 否则「删一条」会变成一次独立的即时提交，而写入是攒批的——
	// 两者交错时，先攒的写入会被后到的删除提交覆盖掉。
	pdeleted map[string]map[string]bool
}

// Open 打开一个裸仓库。目录不存在时报错，不自动创建——
// 「项目存在」是一个需要人明确决定的事，不该由一次读操作顺手造就。
func Open(path string) (*Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("仓库 %s: %w", abs, err)
	}
	for _, want := range []string{"HEAD", "objects"} {
		if _, err := os.Stat(filepath.Join(abs, want)); err != nil {
			return nil, fmt.Errorf("%s 不是裸仓库（缺 %s）", abs, want)
		}
	}
	r := &Repo{Path: abs}
	// 上次进程如果被强杀，这里把没来得及折叠进树的内容捡回来。
	// 失败不影响打开——最坏情况是那一批写入留在悬空对象里。
	_, _ = r.ReplayWAL()
	return r, nil
}

// Init 初始化一个裸仓库。全部用标准库，不调 git。
func Init(path string) (*Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "objects", "info"), 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "objects", "pack"), 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "refs", "heads"), 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "refs", "tags"), 0o750); err != nil {
		return nil, err
	}
	// HEAD 指向默认分支
	head := filepath.Join(abs, "HEAD")
	if _, err := os.Stat(head); err != nil {
		if err := os.WriteFile(head, []byte("ref: refs/heads/zero\n"), 0o644); err != nil {
			return nil, err
		}
	}
	cfg := filepath.Join(abs, "config")
	if _, err := os.Stat(cfg); err != nil {
		body := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"
		if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	desc := filepath.Join(abs, "description")
	if _, err := os.Stat(desc); err != nil {
		_ = os.WriteFile(desc, []byte("zero-memcp memory store\n"), 0o644)
	}
	return Open(abs)
}

// ── refs ──────────────────────────────────────────────────────

func (r *Repo) refPath(ref string) string {
	return filepath.Join(r.Path, filepath.FromSlash(ref))
}

// Refs 列出全部 ref，按名字排序。
func (r *Repo) Refs(prefix string) ([]string, error) {
	var out []string
	root := filepath.Join(r.Path, "refs")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
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
			out = append(out, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Resolve 把一个 ref 名解析成 commit SHA。解析不到返回空串与 nil。
func (r *Repo) Resolve(ref string) (string, error) {
	// 允许简写：zero → refs/heads/zero
	cands := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		cands = append([]string{"refs/heads/" + ref}, cands...)
	}
	for _, c := range cands {
		b, err := os.ReadFile(r.refPath(c))
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
	if b, err := os.ReadFile(filepath.Join(r.Path, "packed-refs")); err == nil {
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

// updateRef 原子地更新一个 ref。
func (r *Repo) updateRef(ref, sha string) error {
	p := r.refPath(ref)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".lock"
	if err := os.WriteFile(tmp, []byte(sha+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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

// List 列出某个分支下的全部条目。
func (r *Repo) List(ref string) ([]Entry, error) {
	c, err := r.commitOf(ref)
	if err != nil {
		return nil, err
	}
	// 分支还没有任何提交时不能提前返回：待提交区可能躺着刚写完
	// 就被强杀的内容，它们必须出现在列表里（也因此会进倒排索引）。
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

	// 合并待提交区
	seen := map[string]bool{}
	for _, e := range list {
		seen[e.Path] = true
	}
	normRef := normalizeRef(ref)
	r.mu.Lock()
	// 待提交的删除先从列表里剔除
	if gone := r.pdeleted[normRef]; len(gone) > 0 {
		filtered := list[:0]
		for _, e := range list {
			if !gone[e.Path] {
				filtered = append(filtered, e)
			}
		}
		list = filtered
	}
	if refMap, ok := r.pending[normRef]; ok {
		for p, data := range refMap {
			if !seen[p] {
				list = append(list, Entry{
					Path: p,
					Mode: r.pmode[normRef][p],
					Hash: sha1OfBlob(data),
					Size: int64(len(data)),
					// 用对象文件的 mtime 当作这条记忆的时间。
					// 崩溃重启后它必须还在——否则召回权重里「越新越重」
					// 会把刚写的记忆按 1970 年算，排到最后。
					Time: r.objectTime(sha1OfBlob(data)),
				})
			}
		}
	}
	r.mu.Unlock()
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

// Read 读出一个文件的全部内容。
func (r *Repo) Read(ref, path string) ([]byte, error) {
	// 待提交区优先：刚写下还没提交的内容也必须读得到。
	// 记忆服务最常见的动作就是「写一条，马上读回来确认」。
	normRef := normalizeRef(ref)
	r.mu.Lock()
	if refMap, ok := r.pending[normRef]; ok {
		if data, ok := refMap[path]; ok {
			cp := make([]byte, len(data))
			copy(cp, data)
			r.mu.Unlock()
			return cp, nil
		}
	}
	r.mu.Unlock()

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
			continue
		}
		out[p] = b
	}
	return out, nil
}

// ── 写 ────────────────────────────────────────────────────────

// Write 把内容写进仓库。
//
// 顺序是三步，任何一步失败都不会留下「看起来成功」的假象：
//
//	① 内容写到临时文件，fsync，再 rename 到 objects/xx/yyyy
//	   rename 是原子的，所以磁盘上永远不会出现半个对象；
//	   已存在同 SHA 则直接跳过（内容寻址，同一份内容就是同一个对象）
//	② 往索引追加一行「哪个分支的哪个路径指向哪个 blob」
//	   这一步失败时 blob 成为孤儿——内容在盘上，但没人指向它，
//	   而调用方拿到错误，所以不会有人以为写成功了
//	③ 最后才更新内存待提交区
//	   内存是最后一步，保证它反映的一定是已经落盘的状态
//
// 于是 kill -9 在任何一点都会落进这两种情况之一：
// 要么这一条根本没写成功（调用方拿到错误），要么它完整地在盘上。
//
// 注意：倒排索引不在这里。它是纯派生物，启动时从 blob 重建，
// 不落盘也不上传——持久化的只有内容（blob）和指向它的记录（索引行）。
func (r *Repo) Write(ref, path string, data []byte, mode Mode) error {
	if err := validPath(path); err != nil {
		return err
	}
	if mode == "" {
		mode = ModeContent
	}

	// ① blob 落盘（临时文件 → fsync → rename）
	sha, err := r.writeObject("blob", data)
	if err != nil {
		return err
	}

	// ② 索引追加
	if err := r.appendWAL(ref, path, mode, sha); err != nil {
		return err
	}

	// ③ 内存
	normRef := normalizeRef(ref)
	cp := make([]byte, len(data))
	copy(cp, data)
	r.mu.Lock()
	if r.pending == nil {
		r.pending = map[string]map[string][]byte{}
		r.pmode = map[string]map[string]Mode{}
	}
	if r.pending[normRef] == nil {
		r.pending[normRef] = map[string][]byte{}
		r.pmode[normRef] = map[string]Mode{}
	}
	r.pending[normRef][path] = cp
	r.pmode[normRef][path] = mode
	r.mu.Unlock()
	return nil
}

// Pending 返回某分支（或全部分支）待提交的条数。
func (r *Repo) Pending(ref ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(ref) > 0 && ref[0] != "" {
		norm := normalizeRef(ref[0])
		return len(r.pending[norm])
	}
	total := 0
	for _, m := range r.pending {
		total += len(m)
	}
	return total
}

// Commit 把待提交区写成对象并推进 ref。
func (r *Repo) Commit(ref, message string) (string, error) {
	normRef := normalizeRef(ref)
	r.mu.Lock()
	pending := r.pending[normRef]
	pmode := r.pmode[normRef]
	if len(pending) == 0 && len(r.pdeleted[normRef]) == 0 {
		r.mu.Unlock()
		return "", nil
	}
	deleted := r.pdeleted[normRef]
	delete(r.pending, normRef)
	delete(r.pmode, normRef)
	delete(r.pdeleted, normRef)
	r.mu.Unlock()

	restore := func() {
		r.mu.Lock()
		if r.pending == nil {
			r.pending = map[string]map[string][]byte{}
			r.pmode = map[string]map[string]Mode{}
		}
		if r.pending[normRef] == nil {
			r.pending[normRef] = map[string][]byte{}
			r.pmode[normRef] = map[string]Mode{}
		}
		for p, d := range pending {
			r.pending[normRef][p] = d
			r.pmode[normRef][p] = pmode[p]
		}
		if len(deleted) > 0 {
			if r.pdeleted == nil {
				r.pdeleted = map[string]map[string]bool{}
			}
			r.pdeleted[normRef] = deleted
		}
		r.mu.Unlock()
	}

	// ① 每个文件写一个 blob（这一步就已经永久落盘）
	node := newTreeNode()
	oldCommit, err := r.Resolve(ref)
	if err != nil {
		restore()
		return "", err
	}

	// 先把旧树整个载入，再叠加新条目——只写新条目会丢掉历史内容
	if oldCommit != "" {
		old, err := r.commitOf(ref)
		if err != nil {
			restore()
			return "", err
		}
		if old.Tree != "" {
			flat := map[string]Entry{}
			if err := r.loadTree(old.Tree, "", flat); err != nil {
				restore()
				return "", err
			}
			gone := deleted
			for p, e := range flat {
				if _, replacing := pending[p]; replacing {
					continue
				}
				if gone[p] {
					continue
				}
				if err := node.insert(p, string(e.Mode), e.Hash); err != nil {
					restore()
					return "", err
				}
			}
		}
	}

	// 再写入这一批
	for p, data := range pending {
		sha, err := r.writeObject("blob", data)
		if err != nil {
			restore()
			return "", fmt.Errorf("写入 blob %s: %w", p, err)
		}
		if err := node.insert(p, string(pmode[p]), sha); err != nil {
			restore()
			return "", err
		}
	}

	treeSHA, err := node.write(r)
	if err != nil {
		restore()
		return "", fmt.Errorf("写树: %w", err)
	}

	var parents []string
	if oldCommit != "" {
		parents = []string{oldCommit}
	}
	commitSHA, err := r.writeCommit(treeSHA, parents, message, time.Now())
	if err != nil {
		restore()
		return "", fmt.Errorf("写提交: %w", err)
	}

	if err := r.updateRef("refs/heads/"+normalizeRef(ref), commitSHA); err != nil {
		restore()
		return "", fmt.Errorf("更新 ref: %w", err)
	}
	// ref 已经指向新提交，WAL 里那批记录到此才算数完了。
	_ = r.advanceCheckpoint(ref)
	return commitSHA, nil
}

func normalizeRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// objectTime 取一个对象文件的修改时间（Unix 秒），取不到返回 0。
func (r *Repo) objectTime(sha string) int64 {
	if err := validSHA(sha); err != nil {
		return 0
	}
	info, err := os.Stat(r.objectPath(sha))
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
	sha, err := r.Resolve(from)
	if err != nil {
		return err
	}
	if sha == "" {
		return fmt.Errorf("分支 %s 不存在", from)
	}
	// 重置/分叉时把目标分支的待提交区清掉：重置就是放弃所有脏状态，
	// 继承源分支的纯净快照。不清掉的话，之前没提交的写入会叠加在
	// 新快照上，看起来像「没重置干净」。
	normTo := normalizeRef(to)
	r.mu.Lock()
	if r.pending != nil {
		delete(r.pending, normTo)
	}
	if r.pmode != nil {
		delete(r.pmode, normTo)
	}
	if r.pdeleted != nil {
		delete(r.pdeleted, normTo)
	}
	r.mu.Unlock()
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

var _ = strconv.Itoa

// ── 索引日志（WAL） ────────────────────────────────────────────
//
// 这一层记录「哪些 blob 还没进树」。没有它，进程被杀之后那些 blob
// 就成了悬空对象——内容还在磁盘上，但谁也找不到它。
//
// 三个设计约束，都是为了「随时可能被杀」：
//
//   - 只追加，永不重写。重写意味着「写新文件 + rename」，而 rename
//     之前的那一瞬间被杀会留下半成品，最坏情况让原日志消失。
//     数据库的做法是 checkpoint：日志只涨，进度记在别处。
//
//   - 每行带 CRC32。断电会让最后一行只写了一半，而半行会和下一条
//     记录拼在一起——没有校验的话，一条断电记录能毁掉两条正常记录。
//
//   - 重放遇到坏行就停。坏行之后的内容不可信（可能已经串行），
//     继续往下读只会把垃圾当数据。

// walPath 是索引日志的位置。
func (r *Repo) walPath() string { return filepath.Join(r.Path, "zero-memcp.wal") }

// ckptPath 记录「日志的哪些字节已经折叠进树了」。
func (r *Repo) ckptPath() string { return filepath.Join(r.Path, "zero-memcp.checkpoint") }

// walLine 拼一行日志：crc \t ref \t mode \t sha \t path
func walLine(ref, path string, mode Mode, sha string) string {
	body := strings.Join([]string{normalizeRef(ref), string(mode), sha, path}, "\t")
	return fmt.Sprintf("%08x\t%s\n", crc32.ChecksumIEEE([]byte(body)), body)
}

// appendWAL 追加一条记录并 fsync。
func (r *Repo) appendWAL(ref, path string, mode Mode, sha string) error {
	f, err := os.OpenFile(r.walPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开索引日志: %w", err)
	}
	if _, err := f.WriteString(walLine(ref, path, mode, sha)); err != nil {
		f.Close()
		return fmt.Errorf("写索引日志: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步索引日志: %w", err)
	}
	return f.Close()
}

// readCheckpoint 读出「日志前 N 字节已折叠」。
func (r *Repo) readCheckpoint() int64 {
	b, err := os.ReadFile(r.ckptPath())
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeCheckpoint 原子地推进进度。
//
// 先写临时文件再 rename：rename 之前被杀，旧的 checkpoint 还在，
// 最多重放一遍已提交的记录——而重放是幂等的（只是重新指向同一个 blob）。
func (r *Repo) writeCheckpoint(offset int64) error {
	p := r.ckptPath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)+"\n"), 0o644); err != nil {
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	return os.Rename(tmp, p)
}

// walEntry 是重放出来的一条记录。
type walEntry struct {
	Ref  string
	Mode Mode
	SHA  string
	Path string
}

// scanWAL 从 offset 开始读日志，返回合法记录与「下一段的起始偏移」。
//
// 遇到校验不过的行就停——后面不可信。返回的 end 是最后一条合法记录
// 之后的字节位置，checkpoint 就写这个值。
func (r *Repo) scanWAL(offset int64) (entries []walEntry, end int64, err error) {
	f, err := os.Open(r.walPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}

	pos := offset
	for {
		nl := bytes.IndexByte(raw, '\n')
		if nl < 0 {
			// 末尾没有换行 = 被截断的半行，丢弃。
			// 不推进 pos，下次启动还会再读一遍（幂等）。
			break
		}
		line := raw[:nl]
		raw = raw[nl+1:]
		lineStart := pos
		pos += int64(nl + 1)

		crcStr, body, ok := bytes.Cut(line, []byte("\t"))
		if !ok || len(crcStr) != 8 {
			pos = lineStart
			break
		}
		want, perr := strconv.ParseUint(string(crcStr), 16, 32)
		if perr != nil {
			pos = lineStart
			break
		}
		if crc32.ChecksumIEEE(body) != uint32(want) {
			// 校验失败：断电污染的记录，停在这里
			pos = lineStart
			break
		}
		fields := strings.Split(string(body), "\t")
		if len(fields) != 4 {
			pos = lineStart
			break
		}
		entries = append(entries, walEntry{Ref: fields[0], Mode: Mode(fields[1]), SHA: fields[2], Path: fields[3]})
	}
	return entries, pos, nil
}

// ReplayWAL 把 checkpoint 之后的记录放回待提交区。
//
// 启动时调用一次。这样「写完立刻被杀」的记忆不会丢——内容在 blob 里，
// 指向它的记录在日志里，重放就能把它们重新挂回树上。
func (r *Repo) ReplayWAL() (int, error) {
	entries, end, err := r.scanWAL(r.readCheckpoint())
	if err != nil {
		return 0, err
	}

	// 把日志截到最后一条合法记录之后。
	//
	// 必须做，不能只是「读的时候跳过」：末尾那半行没有换行符，
	// 下一次追加会直接接在它后面，把半行和新记录拼成一行——
	// 于是新记录也校验失败。一条断电记录会因此持续毒化后续所有写入，
	// 而且是在重启之后才发作，排查时完全看不出因果。
	if info, statErr := os.Stat(r.walPath()); statErr == nil && info.Size() > end {
		if truncErr := os.Truncate(r.walPath(), end); truncErr != nil {
			return 0, fmt.Errorf("修复索引日志尾部: %w", truncErr)
		}
	}
	n := 0
	for _, e := range entries {
		if err := validPath(e.Path); err != nil {
			continue
		}
		typ, body, err := r.readObject(e.SHA)
		if err != nil || typ != "blob" {
			// blob 不在了（被回收或从没写成）——这条记录作废
			continue
		}
		norm := normalizeRef(e.Ref)
		r.mu.Lock()
		if r.pending == nil {
			r.pending = map[string]map[string][]byte{}
			r.pmode = map[string]map[string]Mode{}
		}
		if r.pending[norm] == nil {
			r.pending[norm] = map[string][]byte{}
			r.pmode[norm] = map[string]Mode{}
		}
		r.pending[norm][e.Path] = body
		r.pmode[norm][e.Path] = e.Mode
		r.mu.Unlock()
		n++
	}
	return n, nil
}

// advanceCheckpoint 在某个分支提交成功后推进日志进度。
//
// 只有当「日志里的记录全部属于这个分支」时才能推进——
// 否则别的分支还没提交的记录会被一并跳过，那是真的丢数据。
func (r *Repo) advanceCheckpoint(ref string) error {
	norm := normalizeRef(ref)
	entries, end, err := r.scanWAL(0)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if normalizeRef(e.Ref) == norm {
			continue
		}
		// 还有别的分支未提交：不推进。本分支的记录会留在日志里，
		// 下次重放时重新指向同一个 blob——幂等，不会出错。
		return nil
	}
	return r.writeCheckpoint(end)
}

// CompactWAL 在日志远大于有效内容时收缩一次。
//
// 正常路径走不到这里：checkpoint 让日志只涨不读，一天几百条记录
// 一年也就几 MB。真到了要收缩的时候，用临时文件 + rename。
func (r *Repo) CompactWAL() error {
	off := r.readCheckpoint()
	b, err := os.ReadFile(r.walPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if off <= 0 || int64(len(b)) <= off*2 {
		return nil
	}
	tmp := r.walPath() + ".tmp"
	if err := os.WriteFile(tmp, b[off:], 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.walPath()); err != nil {
		return err
	}
	return r.writeCheckpoint(0)
}

// Delete 删除一个路径。走待提交区，与写入一样的批处理语义。
//
// 删除是「打墓碑 + 记账」，不是立刻改树：这样它和写入共享同一个
// commit 边界，二者不会互相覆盖。
func (r *Repo) Delete(ref, path string) error {
	if err := validPath(path); err != nil {
		return err
	}
	normRef := normalizeRef(ref)
	r.mu.Lock()
	if r.pdeleted == nil {
		r.pdeleted = map[string]map[string]bool{}
	}
	if r.pdeleted[normRef] == nil {
		r.pdeleted[normRef] = map[string]bool{}
	}
	r.pdeleted[normRef][path] = true
	// 同一路径如果之前排着写入，取消它——删就是删。
	// 注意：这只对「同一个分支」成立。跨分支移动时，目标分支的写入
	// 在另一个 key 下，不受影响。
	if r.pending[normRef] != nil {
		delete(r.pending[normRef], path)
	}
	r.mu.Unlock()
	return nil
}

// Deleted 返回某分支待提交的删除路径。
func (r *Repo) Deleted(ref string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for p := range r.pdeleted[normalizeRef(ref)] {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ── tag ───────────────────────────────────────────────────────

// Tags 列出所有 tag，返回 name -> commit SHA。
func (r *Repo) Tags() (map[string]string, error) {
	out := map[string]string{}
	root := filepath.Join(r.Path, "refs", "tags")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
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
		b, rerr := os.ReadFile(p)
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
	if b, rerr := os.ReadFile(filepath.Join(r.Path, "packed-refs")); rerr == nil {
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
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("tag %q 不存在", name)
	}
	dstDir := filepath.Join(r.Path, "refs", "tags-removed")
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(dstDir, strings.ReplaceAll(name, "/", "_")+"."+stamp)
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return err
	}
	return os.Remove(src)
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
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dstDir, name), []byte(sha+"\n"), 0o644); err != nil {
		return err
	}
	return os.Remove(filepath.Join(r.Path, "refs", "heads", name))
}

// Branches 列出该仓库所有的分支名（refs/heads/ 下的短名）。
func (r *Repo) Branches() []string {
	var out []string
	root := filepath.Join(r.Path, "refs", "heads")
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr == nil && rel != "" {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// PendingRefs 列出当前待提交区里有变动的分支名。
func (r *Repo) PendingRefs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for ref, m := range r.pending {
		if len(m) > 0 {
			out = append(out, ref)
		}
	}
	for ref, m := range r.pdeleted {
		if len(m) > 0 {
			out = append(out, ref)
		}
	}
	return out
}
