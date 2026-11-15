package index

import (
	"compress/gzip"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
)

// 索引快照：把「读 25,000 个文件 + 分词」的结果存下来，下次启动直接加载。
//
// # 为什么需要它
//
// 冷启动实测 33 秒，其中 23 秒是扫描（读盘判 frontmatter）、9 秒是分词。
// 这两件事的**结果**是确定的：同一份仓库内容，分出来就是同一份倒排表。
// 每次发版都重算一遍，等于让用户为「我们换了二进制」付 33 秒的不可用。
//
// # 为什么不干脆信任快照
//
// 快照可能过期（仓库被外部改动、快照写了一半、格式变了）。
// 所以它带一个头部校验和 + 每个仓库的 head 记录：
//   - 头部对不上 → 整份丢弃，走全量重建
//   - 某个仓库的 head 变了 → 只重建那条分支（已有的增量路径）
//   - 一切正常 → 直接用
//
// 换句话说：快照是**优化**，全量重建仍是正确性保底。
// 这条边界必须守住——否则会出现「启动很快，但搜不到刚写的东西」。
const (
	snapshotMagic   = "zmidx1"
	snapshotVersion = 1
)

// SnapshotHeader 是快照的元信息，与倒排表分开存放。
//
// 分开的原因：加载时先只读头部（几 KB），校验通过再决定要不要
// 反序列化那几十 MB 的正文。头部不匹配时省掉整个读盘过程。
type SnapshotHeader struct {
	Magic   string
	Version int
	// Heads 是快照生成时每个「项目@分支」的 head SHA。
	// 加载后逐个比对，变了的那条分支走增量重建。
	Heads map[string]string
	// Docs / Terms 用于判断快照是否明显不完整。
	Docs  int
	Terms int
	// TreeFile 是树快照的文件名（与倒排表分开存，因为它可以独立失效）。
	TreeFile string
}

// SnapshotData 是倒排表的可序列化形态。
//
// 字段必须是导出的，且只放值类型——gob 不解引用指针，
// 用指针会让快照带上内存布局，换个版本就读不出来。
type SnapshotData struct {
	Postings  map[string]map[string]int32
	DocLen    map[string]int32
	DocSize   map[string]int32
	DocTime   map[string]int64
	DocTokens map[string][]string
	TotalDocs int
}

// Export 把索引导出成可序列化形态。
func (ix *Index) Export() *SnapshotData {
	d := &SnapshotData{
		Postings:  make(map[string]map[string]int32, len(ix.postings)),
		DocLen:    make(map[string]int32, len(ix.docLen)),
		DocSize:   make(map[string]int32, len(ix.docSize)),
		DocTime:   make(map[string]int64, len(ix.docTime)),
		DocTokens: make(map[string][]string, len(ix.docTokens)),
		TotalDocs: ix.totalDocs,
	}
	for tok, docs := range ix.postings {
		m := make(map[string]int32, len(docs))
		for addr, tf := range docs {
			m[addr] = int32(tf)
		}
		d.Postings[string(tok)] = m
	}
	for addr, n := range ix.docLen {
		d.DocLen[addr] = int32(n)
	}
	for addr, n := range ix.docSize {
		d.DocSize[addr] = int32(n)
	}
	for addr, t := range ix.docTime {
		d.DocTime[addr] = t
	}
	for addr, toks := range ix.docTokens {
		s := make([]string, len(toks))
		for i, tk := range toks {
			s[i] = string(tk)
		}
		d.DocTokens[addr] = s
	}
	return d
}

// Import 从快照重建索引。
func Import(d *SnapshotData) *Index {
	ix := New()
	ix.totalDocs = d.TotalDocs
	for tok, docs := range d.Postings {
		m := make(map[string]int, len(docs))
		for addr, tf := range docs {
			m[addr] = int(tf)
		}
		ix.postings[Token(tok)] = m
	}
	for addr, n := range d.DocLen {
		ix.docLen[addr] = int(n)
	}
	for addr, n := range d.DocSize {
		ix.docSize[addr] = int(n)
	}
	for addr, t := range d.DocTime {
		ix.docTime[addr] = t
	}
	for addr, toks := range d.DocTokens {
		ts := make([]Token, len(toks))
		for i, s := range toks {
			ts[i] = Token(s)
		}
		ix.docTokens[addr] = ts
	}
	return ix
}

// SaveSnapshot 原子写一份快照到目录。
//
// 先写 .tmp 再 rename：进程在写一半时被杀掉，留下的是半个文件；
// 而 rename 在同一文件系统内是原子的，所以要么是完整旧版、要么是完整新版。
// 这是「重启随时可能发生」这个前提下唯一安全的写法。
func SaveSnapshot(dir string, hdr *SnapshotHeader, data *SnapshotData) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	hdr.Magic = snapshotMagic
	hdr.Version = snapshotVersion

	hdrPath := filepath.Join(dir, "index-head.gob")
	dataPath := filepath.Join(dir, "index-data.gob")

	if err := writeGobAtomic(hdrPath, hdr); err != nil {
		return fmt.Errorf("write snapshot header: %w", err)
	}
	// data 为 nil 表示倒排表由 WriteBinary 单独写（紧凑格式）。
	// 早先这里无条件写 gob，传 nil 直接 panic：gob 不能编码 nil 指针。
	if data != nil {
		if err := writeGobAtomic(dataPath, data); err != nil {
			return fmt.Errorf("write snapshot data: %w", err)
		}
	}
	return nil
}

// LoadSnapshotHeader 只读头部。
func LoadSnapshotHeader(dir string) (*SnapshotHeader, error) {
	f, err := os.Open(filepath.Join(dir, "index-head.gob"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, zerr := gzip.NewReader(f)
	if zerr != nil {
		return nil, fmt.Errorf("snapshot header is not gzip: %w", zerr)
	}
	defer zr.Close()
	var h SnapshotHeader
	if err := gob.NewDecoder(zr).Decode(&h); err != nil {
		return nil, fmt.Errorf("decode snapshot header: %w", err)
	}
	if h.Magic != snapshotMagic {
		return nil, fmt.Errorf("snapshot magic mismatch (%q)", h.Magic)
	}
	if h.Version != snapshotVersion {
		return nil, fmt.Errorf("snapshot version %d unsupported (want %d)", h.Version, snapshotVersion)
	}
	return &h, nil
}

// LoadSnapshotData 读倒排表正文。
func LoadSnapshotData(dir string) (*SnapshotData, error) {
	f, err := os.Open(filepath.Join(dir, "index-data.gob"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, zerr := gzip.NewReader(f)
	if zerr != nil {
		return nil, fmt.Errorf("snapshot data is not gzip: %w", zerr)
	}
	defer zr.Close()
	var d SnapshotData
	if err := gob.NewDecoder(zr).Decode(&d); err != nil {
		return nil, fmt.Errorf("decode snapshot data: %w", err)
	}
	return &d, nil
}

// writeGobAtomic 原子写一份 gob，并做 gzip 压缩。
//
// 为什么要压缩：倒排表里同一个块地址（project@branch:path）会在
// 它出现过的每个词下面重复一次。实测 25,000 篇解压前 660 MB，
// 这种高度重复的字符串压缩比通常十几倍——不压就是每次退出写 660MB。
// 压缩的 CPU 代价远小于磁盘 I/O。
func writeGobAtomic(path string, v any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err := gob.NewEncoder(zw).Encode(v); err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// AllDocs 返回索引里全部文档 ID 的集合（校验快照与树是否对得上用）。
func (ix *Index) AllDocs() map[string]struct{} {
	out := make(map[string]struct{}, ix.totalDocs)
	for addr := range ix.docLen {
		out[addr] = struct{}{}
	}
	return out
}

// TreeSnapshot 是一条分支的内存树的可序列化形态。
//
// 为什么要存它：启动慢的两段是「扫描 23s」和「分词 9s」。
// 只存倒排表只省了后者。而树里的每个 Block 除了 Read 之外全是值——
// Read 就是 repo.Read(branch, path)，可以从 (project, branch, path)
// 重建。所以整棵树也能存，存完启动就只剩「读文件 + 建闭包」。
//
// 存的是**判定结果**（Rule / Hidden），不是判定依据：
// frontmatter 解析和规则目录传播都已经算过，重算没有意义。
type TreeSnapshot struct {
	Project string
	Branch  string
	Blocks  []BlockSnapshot
}

// BlockSnapshot 是一个 Block 去掉 Read 闭包之后的样子。
type BlockSnapshot struct {
	Path   string
	Size   int64
	Time   int64
	Rule   bool
	Hidden bool
	Hash   string
}

// SaveTrees 把树快照写到 dir 下的独立文件。
//
// 与倒排表分开的原因：两者的失效条件不同。倒排表只在「内容变了」
// 时失效，树还在「路径集合变了」时失效——分开存可以让「只加了一个
// 空文件」这种情况只重扫树、不重算分词。
func SaveTrees(dir string, trees []TreeSnapshot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeGobAtomic(filepath.Join(dir, "index-trees.gob"), trees)
}

// LoadTrees 读树快照。
func LoadTrees(dir string) ([]TreeSnapshot, error) {
	f, err := os.Open(filepath.Join(dir, "index-trees.gob"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, zerr := gzip.NewReader(f)
	if zerr != nil {
		return nil, fmt.Errorf("tree snapshot is not gzip: %w", zerr)
	}
	defer zr.Close()
	var ts []TreeSnapshot
	if err := gob.NewDecoder(zr).Decode(&ts); err != nil {
		return nil, fmt.Errorf("decode tree snapshot: %w", err)
	}
	return ts, nil
}
