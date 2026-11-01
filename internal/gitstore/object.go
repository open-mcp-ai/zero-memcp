package gitstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 这一层直接用 Go 标准库读写 Git 的松散对象，不调用 git 命令。
//
// 为什么不调 git：
//   - 每次写要 fork 一个进程，20~50ms，而一次写入不该有这个开销
//   - 依赖机器上装着 git，而记忆服务应该是一个可以直接跑的单二进制
//
// 为什么仍然用 Git 的对象格式：
//   - 内容寻址（SHA-1）本身就是幂等与去重的实现
//   - 外面的人用 `git log` / `git clone` 依然能看这个仓库
//   - 格式简单到用标准库三十行就能写对

// objectPath 返回一个对象在松散对象库里的路径。
func (r *Repo) objectPath(sha string) string {
	return filepath.Join(r.Path, "objects", sha[:2], sha[2:])
}

// writeObject 把一个对象写进松散对象库，返回它的 SHA-1。
//
// 已存在就直接返回——对象按内容寻址，同一个 SHA 必然是同一份内容，
// 重写没有意义，也没有「覆盖」这回事。
func (r *Repo) writeObject(typ string, content []byte) (string, error) {
	header := fmt.Sprintf("%s %d\x00", typ, len(content))
	full := make([]byte, 0, len(header)+len(content))
	full = append(full, header...)
	full = append(full, content...)

	sum := sha1.Sum(full)
	sha := hex.EncodeToString(sum[:])
	dst := r.objectPath(sha)
	if _, err := r.FS().Stat(dst); err == nil {
		return sha, nil
	}

	if err := r.FS().MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("建对象目录: %w", err)
	}

	// 压缩到临时文件再 rename：通过 FS.WriteAtomicWithSync 原子同步
	err := r.FS().WriteAtomicWithSync(dst, 0o444, func(w io.Writer) error {
		zw := zlib.NewWriter(w)
		if _, err := zw.Write(full); err != nil {
			return fmt.Errorf("压缩对象: %w", err)
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("关闭压缩流: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("落盘对象: %w", err)
	}
	return sha, nil
}

// readObject 读出一个对象，返回类型与内容。
func (r *Repo) readObject(sha string) (string, []byte, error) {
	if err := validSHA(sha); err != nil {
		return "", nil, err
	}
	f, err := r.FS().Open(r.objectPath(sha))
	if err != nil {
		if isNotExist(err) {
			// 松散对象库没有：退回查 packfile。
			// 任何跑过 gc 或刚 clone 的仓库，对象都在 pack 里。
			return r.readPacked(sha)
		}
		return "", nil, fmt.Errorf("打开对象 %s: %w", sha, err)
	}
	defer f.Close()

	zr, err := zlib.NewReader(f)
	if err != nil {
		return "", nil, fmt.Errorf("解压对象 %s: %w", sha, err)
	}
	defer zr.Close()

	raw, err := io.ReadAll(zr)
	if err != nil {
		return "", nil, fmt.Errorf("读取对象 %s: %w", sha, err)
	}
	nul := bytes.IndexByte(raw, 0)
	if nul < 0 {
		return "", nil, fmt.Errorf("对象 %s 头部损坏", sha)
	}
	head := string(raw[:nul])
	typ, sizeStr, ok := strings.Cut(head, " ")
	if !ok {
		return "", nil, fmt.Errorf("对象 %s 头部格式不对: %q", sha, head)
	}
	size, err := strconv.Atoi(sizeStr)
	if err != nil {
		return "", nil, fmt.Errorf("对象 %s 大小字段损坏: %q", sha, sizeStr)
	}
	body := raw[nul+1:]
	if len(body) != size {
		return "", nil, fmt.Errorf("对象 %s 长度不符：声明 %d，实际 %d", sha, size, len(body))
	}
	return typ, body, nil
}

func validSHA(sha string) error {
	if len(sha) != 40 {
		return fmt.Errorf("SHA 应为 40 位十六进制，得到 %q", sha)
	}
	for _, c := range sha {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return fmt.Errorf("SHA 只能是小写十六进制，得到 %q", sha)
		}
	}
	return nil
}

// treeItem 是树里的一项。
type treeItem struct {
	mode string
	name string
	sha  string
}

// parseTree 解析一个 tree 对象。
func parseTree(body []byte) ([]treeItem, error) {
	var items []treeItem
	for len(body) > 0 {
		sp := bytes.IndexByte(body, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("tree 条目缺少空格")
		}
		mode := string(body[:sp])
		body = body[sp+1:]

		nul := bytes.IndexByte(body, 0)
		if nul < 0 {
			return nil, fmt.Errorf("tree 条目缺少 NUL")
		}
		name := string(body[:nul])
		body = body[nul+1:]

		if len(body) < 20 {
			return nil, fmt.Errorf("tree 条目 %q 的 SHA 不完整", name)
		}
		sha := hex.EncodeToString(body[:20])
		body = body[20:]

		items = append(items, treeItem{mode: mode, name: name, sha: sha})
	}
	return items, nil
}

// serializeTree 把树条目序列化成 tree 对象的二进制内容。
//
// git 的排序规则：目录名按「加了 / 之后」的字节序参与排序。
// 写成普通字符串排序的话，`a.md` 与目录 `a` 的相对顺序会错，
// 而错序的 tree 会让 git fsck 报错、也会让 clone 出来的历史与预期不符。
func serializeTree(items []treeItem) []byte {
	sorted := make([]treeItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		ni, nj := sorted[i].name, sorted[j].name
		if sorted[i].mode == "40000" {
			ni += "/"
		}
		if sorted[j].mode == "40000" {
			nj += "/"
		}
		return ni < nj
	})

	var buf bytes.Buffer
	for _, it := range sorted {
		buf.WriteString(it.mode)
		buf.WriteByte(' ')
		buf.WriteString(it.name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(it.sha)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// treeNode 是构建树时的内存节点。
type treeNode struct {
	blobs map[string]treeItem
	dirs  map[string]*treeNode
}

func newTreeNode() *treeNode {
	return &treeNode{blobs: map[string]treeItem{}, dirs: map[string]*treeNode{}}
}

// insert 把一条 (路径, 模式, SHA) 放进树里。
func (n *treeNode) insert(path, mode, sha string) error {
	parts := strings.Split(path, "/")
	cur := n
	for i, p := range parts {
		if p == "" {
			return fmt.Errorf("路径含空段: %q", path)
		}
		if i == len(parts)-1 {
			if _, isDir := cur.dirs[p]; isDir {
				return fmt.Errorf("路径 %q 与已有目录冲突", path)
			}
			cur.blobs[p] = treeItem{mode: mode, name: p, sha: sha}
			return nil
		}
		next, ok := cur.dirs[p]
		if !ok {
			if _, isBlob := cur.blobs[p]; isBlob {
				return fmt.Errorf("路径 %q 与已有文件冲突", path)
			}
			next = newTreeNode()
			cur.dirs[p] = next
		}
		cur = next
	}
	return nil
}

// write 递归把节点写成 tree 对象，返回根 tree 的 SHA。
func (n *treeNode) write(r *Repo) (string, error) {
	var items []treeItem
	for name, it := range n.blobs {
		items = append(items, treeItem{mode: it.mode, name: name, sha: it.sha})
	}
	for name, sub := range n.dirs {
		sha, err := sub.write(r)
		if err != nil {
			return "", err
		}
		items = append(items, treeItem{mode: "40000", name: name, sha: sha})
	}
	if len(items) == 0 {
		// 空树是合法对象，SHA 固定为 4b825dc642cb6eb9a060e54bf8d69288fbee4904
		return r.writeObject("tree", nil)
	}
	return r.writeObject("tree", serializeTree(items))
}

// loadTree 把一个 tree（含子树）摊平成 路径 → 条目。
func (r *Repo) loadTree(sha, prefix string, out map[string]Entry) error {
	if sha == "" {
		return nil
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return err
	}
	if typ != "tree" {
		return fmt.Errorf("对象 %s 不是 tree，而是 %s", sha, typ)
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
			if err := r.loadTree(it.sha, full, out); err != nil {
				return err
			}
			continue
		}
		mode := ModeContent
		if it.mode == "100755" {
			mode = ModeRule
		}
		out[full] = Entry{Path: full, Mode: mode, Hash: it.sha}
	}
	return nil
}

// commitObj 是解析出来的提交。
type commitObj struct {
	Tree    string
	Parents []string
	Author  string
	When    time.Time
	Message string
}

// parseCommit 解析 commit 对象。
func parseCommit(body []byte) commitObj {
	var c commitObj
	text := string(body)
	head, msg, _ := strings.Cut(text, "\n\n")
	c.Message = msg

	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimPrefix(line, "tree ")
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimPrefix(line, "parent "))
		case strings.HasPrefix(line, "author "):
			c.Author = strings.TrimPrefix(line, "author ")
			// 形如 "name <email> 1700000000 +0800"
			fields := strings.Fields(c.Author)
			if len(fields) >= 2 {
				if ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
					c.When = time.Unix(ts, 0)
				}
			}
		}
	}
	return c
}

// identity 是写进 commit 的作者与提交者。固定值，不读环境变量——
// 谁写的记在切片的 frontmatter 里（source 字段），不靠 git 的作者字段。
const identity = "zero-memcp <zero-memcp@local>"

// writeCommit 生成一个 commit 对象，返回它的 SHA。
func (r *Repo) writeCommit(treeSHA string, parents []string, message string, when time.Time) (string, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", treeSHA)
	for _, p := range parents {
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	_, offset := when.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	tz := fmt.Sprintf("%s%02d%02d", sign, offset/3600, (offset%3600)/60)
	stamp := when.Unix()

	fmt.Fprintf(&buf, "author %s %d %s\n", identity, stamp, tz)
	fmt.Fprintf(&buf, "committer %s %d %s\n", identity, stamp, tz)
	buf.WriteString("\n")
	buf.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		buf.WriteString("\n")
	}
	return r.writeObject("commit", buf.Bytes())
}

// sha1OfBlob 算出一个 blob 内容的 SHA-1，不落盘。
func sha1OfBlob(data []byte) string {
	header := fmt.Sprintf("blob %d\x00", len(data))
	h := sha1.New()
	h.Write([]byte(header))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
