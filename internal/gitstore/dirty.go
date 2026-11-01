package gitstore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 脏层（dirty overlay）：干净 Git tree 之上，按目录做的一层覆盖。
//
//	干净层  objects/ + refs/    不可变、内容寻址、外部 git 工具直接可读
//	脏层    dirty/<branch>/…    每个被改过的目录一个文件，改了就落盘
//	提交    脏层折叠成 tree 对象 → 推 ref → 删脏文件
//
// 为什么脏文件的名字用「目录路径」而不是「干净 tree 对象的 SHA」：
//   - 内容寻址意味着两个不同目录若内容相同，就共享同一个 tree 对象。
//     按 SHA 命名会让它们的脏文件撞在同一个名字上——改一个等于改两个，
//     而且完全不报错。
//   - 新建的目录在干净层没有任何对象，也就没有 SHA 可以拼名字。
//   - 路径本身是唯一可靠的名字。前面加 dirty/ 之后与 objects/ 彻底分离，
//     git gc / git fsck / 外部 clone 都看不见它，不会被误伤也不会被打包。
//
// 命名规则：目录 d 的脏文件是 `dirty/<branch>/@<d>.dirty`，根是 `@.dirty`。
// 于是目录 "a" 是文件 `@a.dirty`，目录 "a/b" 是 `@a/b.dirty`：一个是文件
// 一个是目录，永不冲突；前缀 @ 与后缀 .dirty 都是常量，反解析唯一。
const (
	dirtyDirName  = "dirty"
	dirtyPrefix   = "@"
	dirtySuffix   = ".dirty"
	dirtyMagic    = "zmd1"
	dirtyOpSet    = 'S' // 改动/新增：这一条要覆盖干净层
	dirtyOpBase   = 'B' // 基线：从干净层复制来的原值，只为可读性与诊断
	dirtyOpDelete = 'D' // 墓碑：这一条要从干净层去掉
)

// dirtyEntry 是脏层里的一条目录项。
type dirtyEntry struct {
	Mode Mode
	SHA  string
}

// dirtyNode 是一个目录的覆盖。
//
// set/del 是**事实**：折叠时只认它们。base 是**参考**：这个目录第一次
// 被改动时，从干净层复制来的原值。为什么两者要分开：
//
//   - 「复制干净节点 → 在脏节点上 append」是这一层的语义，所以基线
//     必须留在文件里——cat 一个脏文件就能看出「原来是什么、改成了什么」。
//   - 但折叠绝不能按基线覆盖：从写入到提交之间，外部可能 push 或
//     直接 git commit 推进了同一个目录。以基线为准就会把别人的新内容
//     静默盖回旧内容。只认 set/del，未提及的名字永远以干净层为准，
//     这类覆盖在结构上就不可能发生。
//
// 删除必须单独记：干净层里那条还在，不记就等于没删。
type dirtyNode struct {
	set  map[string]dirtyEntry
	base map[string]dirtyEntry
	del  map[string]bool
}

func newDirtyNode() *dirtyNode {
	return &dirtyNode{
		set:  map[string]dirtyEntry{},
		base: map[string]dirtyEntry{},
		del:  map[string]bool{},
	}
}

// count 是这个目录里**待折叠的改动数**（不含基线）。
func (n *dirtyNode) count() int { return len(n.set) + len(n.del) }

// dirtyDirPath 返回某分支的脏层根目录。
func (r *Repo) dirtyDirPath(ref string) string {
	return filepath.Join(r.Path, dirtyDirName, normalizeRef(ref))
}

// dirtyFilePath 返回某分支某目录的脏文件路径。dir 是树内斜杠路径，根为 ""。
func (r *Repo) dirtyFilePath(ref, dir string) string {
	dir = strings.Trim(dir, "/")
	name := dirtyPrefix + dir + dirtySuffix
	return filepath.Join(r.dirtyDirPath(ref), filepath.FromSlash(name))
}

// dirtyDirFromFile 把脏文件名反解析成树内目录路径。
func dirtyDirFromFile(rel string) (string, bool) {
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, dirtyPrefix) || !strings.HasSuffix(rel, dirtySuffix) {
		return "", false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(rel, dirtyPrefix), dirtySuffix)
	return strings.TrimPrefix(body, "/"), true
}

// encodeDirtyNode 序列化成文本。可 cat、可 diff，出问题直接看文件就知道。
func encodeDirtyNode(n *dirtyNode) ([]byte, error) {
	var b strings.Builder
	b.WriteString(dirtyMagic + "\n")
	for _, line := range encodeEntries(dirtyOpSet, n.set) {
		b.WriteString(line)
	}
	for _, line := range encodeEntries(dirtyOpBase, n.base) {
		b.WriteString(line)
	}
	var dels []string
	for name := range n.del {
		dels = append(dels, name)
	}
	sort.Strings(dels)
	for _, name := range dels {
		if strings.ContainsAny(name, "\t\n") {
			return nil, fmt.Errorf("脏层项名含制表符或换行：%q", name)
		}
		fmt.Fprintf(&b, "%c\t%s\n", dirtyOpDelete, name)
	}
	return []byte(b.String()), nil
}

// encodeEntries 把一组条目排好序输出。
func encodeEntries(op byte, m map[string]dirtyEntry) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		e := m[name]
		out = append(out, fmt.Sprintf("%c\t%s\t%s\t%s\n", op, string(e.Mode), e.SHA, name))
	}
	return out
}

// decodeDirtyNode 解析脏文件。
//
// 语义降级之后（put 只承诺「已投递」，不承诺落盘）脏文件可能是半截的：
// 没有 fsync 就没有「要么完整、要么没有」的保证。所以这里遇到坏行
// **跳过而不是报错**——半截行最多丢掉一条改动，而报错会让整棵分支的
// 脏层不可读，症状从「少一条记忆」变成「这个仓库打不开」。
func decodeDirtyNode(data []byte) *dirtyNode {
	n := newDirtyNode()
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	first := true
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if first {
			first = false
			if strings.TrimSpace(line) == dirtyMagic {
				continue
			}
			// 头都不对：整个文件不可信。当作空覆盖，干净层仍然是事实。
			return newDirtyNode()
		}
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		switch fields[0][0] {
		case dirtyOpDelete:
			if len(fields) != 2 || fields[1] == "" {
				continue
			}
			n.del[fields[1]] = true
			delete(n.set, fields[1])
			delete(n.base, fields[1])
		case dirtyOpSet, dirtyOpBase:
			if len(fields) != 4 {
				continue
			}
			mode, sha, name := Mode(fields[1]), fields[2], fields[3]
			if name == "" || string(mode) == "" || validSHA(sha) != nil {
				continue
			}
			e := dirtyEntry{Mode: mode, SHA: sha}
			if fields[0][0] == dirtyOpSet {
				n.set[name] = e
				delete(n.del, name)
				continue
			}
			n.base[name] = e
		}
	}
	return n
}

// loadDirtyNode 读一个目录的脏覆盖。文件不存在返回 nil（不是错误）。
func (r *Repo) loadDirtyNode(ref, dir string) (*dirtyNode, error) {
	data, err := r.FS().ReadFile(r.dirtyFilePath(ref, dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return decodeDirtyNode(data), nil
}

// saveDirtyNode 写一个目录的脏覆盖。
//
// 临时文件 + rename，不 fsync：put 的承诺是「已投递」，不是「已落盘」。
// rename 保证读者永远只看到完整的旧版或完整的新版；掉电最多退回上一版，
// 那正是那条承诺允许的后果。
func (r *Repo) saveDirtyNode(ref, dir string, n *dirtyNode) error {
	if n.count() == 0 {
		return r.deleteDirtyNode(ref, dir)
	}
	data, err := encodeDirtyNode(n)
	if err != nil {
		return err
	}
	path := r.dirtyFilePath(ref, dir)
	if err := r.FS().MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	// 0600：脏文件只有本服务自己读，不像 refs 那样需要给外部 git 工具看。
	return r.FS().WriteAtomic(path, data, 0o600)
}

// deleteDirtyNode 删一个目录的脏覆盖。不存在不算错。
func (r *Repo) deleteDirtyNode(ref, dir string) error {
	if err := r.FS().Remove(r.dirtyFilePath(ref, dir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// listDirtyDirs 列出某分支所有**有待折叠改动**的目录（含根 ""），按深度升序。
//
// 只写了基线、没有实际改动的目录不算——那种文件是中间态，折叠它等于
// 把干净层原地抄一遍。
func (r *Repo) listDirtyDirs(ref string) ([]string, error) {
	root := r.dirtyDirPath(ref)
	var out []string
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
		dir, ok := dirtyDirFromFile(rel)
		if !ok {
			return nil
		}
		n, lerr := r.loadDirtyNode(ref, dir)
		if lerr != nil || n == nil || n.count() == 0 {
			return nil
		}
		out = append(out, dir)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortDirtyDirs(out)
	return out, nil
}

// sortDirtyDirs 按深度升序排（浅的在前）。
func sortDirtyDirs(dirs []string) {
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if di != dj {
			return di < dj
		}
		return dirs[i] < dirs[j]
	})
}

// clearDirtyRef 删掉某分支的整个脏层。
func (r *Repo) clearDirtyRef(ref string) error {
	if err := r.FS().RemoveAll(r.dirtyDirPath(ref)); err != nil {
		return fmt.Errorf("清理脏层: %w", err)
	}
	return nil
}

// dirtyRefs 列出脏层里有待折叠改动的分支名。
func (r *Repo) dirtyRefs() ([]string, error) {
	root := filepath.Join(r.Path, dirtyDirName)
	entries, err := r.FS().ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirs, derr := r.listDirtyDirs(e.Name())
		if derr != nil || len(dirs) == 0 {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// lockDirtyFile 取得某个脏目录的文件锁。
func (r *Repo) lockDirtyFile(ref, dir string) func() {
	return r.Locker().LockNode(ref, dir)
}

// startDirtyNode 新建一个目录的覆盖，并把干净层的原值记成基线。
func startDirtyNode(clean []treeItem) *dirtyNode {
	n := newDirtyNode()
	for _, it := range clean {
		n.base[it.name] = dirtyEntry{Mode: Mode(it.mode), SHA: it.sha}
	}
	return n
}

// applyDirty 把待折叠改动叠到干净条目上：set 覆盖同名项，del 移除。
//
// 基线不参与——它是参考，不是意图。
func applyDirty(items []treeItem, n *dirtyNode) []treeItem {
	if n == nil {
		return items
	}
	byName := make(map[string]treeItem, len(items)+len(n.set))
	for _, it := range items {
		byName[it.name] = it
	}
	for name, e := range n.set {
		byName[name] = treeItem{mode: string(e.Mode), name: name, sha: e.SHA}
	}
	for name := range n.del {
		delete(byName, name)
	}
	out := make([]treeItem, 0, len(byName))
	for _, it := range byName {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// cleanItemsAt 从给定根树里读出某个目录的条目。路径不存在时返回空。
func (r *Repo) cleanItemsAt(rootTree, dir string) ([]treeItem, error) {
	sha, err := treeAtSHA(r, rootTree, dir)
	if err != nil {
		return nil, err
	}
	if sha == "" {
		return nil, nil
	}
	return r.treeItems(sha)
}
