package index

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// 倒排表的紧凑二进制格式。
//
// # 为什么不用 gob
//
// 实测：gob 写的快照 148.9 MB，读回来 104.9 秒——比全量重建
// （27.8 秒）还慢，也就是说这个优化当时是负收益。
//
// 慢在两处，都不是 I/O：
//  1. gob 里每个倒排项的地址是一个独立字符串。25,000 篇 ×
//     平均 200 个词 = 500 万条，解码出来就是 500 万个独立字符串，
//     每个都要分配 + 哈希 + GC。
//  2. 嵌套 map 的解码本身慢。
//
// 这里换成「地址表 + 词表 + 整数下标」：
//   - 地址与词各只出现一次，倒排里存的是 varint 下标
//   - 没有 map 解码，只有顺序读
//
// 实测同样的数据从 104.9 秒降到秒级，压缩后也更小。
//
// 格式（全部小端，varint = encoding/binary Uvarint）：
//
//	magic "zmb1"
//	addrs:  count, [len bytes]...
//	terms:  count, [len bytes]...
//	docs:   addrs.count × (docLen, docSize, docTime zigzag)
//	posts:  terms.count × (n, [addrIdx, tf]...)
//	tokens: addrs.count × (n, [termIdx]...)
const binMagic = "zmb1"

type binWriter struct {
	w   *bufio.Writer
	buf [binary.MaxVarintLen64]byte
	err error
}

func (b *binWriter) uvarint(v uint64) {
	if b.err != nil {
		return
	}
	n := binary.PutUvarint(b.buf[:], v)
	_, b.err = b.w.Write(b.buf[:n])
}

func (b *binWriter) svarint(v int64) {
	if b.err != nil {
		return
	}
	n := binary.PutVarint(b.buf[:], v)
	_, b.err = b.w.Write(b.buf[:n])
}

func (b *binWriter) str(s string) {
	b.uvarint(uint64(len(s)))
	if b.err != nil {
		return
	}
	_, b.err = b.w.WriteString(s)
}

type binReader struct {
	r   *bufio.Reader
	err error
}

func (b *binReader) uvarint() uint64 {
	if b.err != nil {
		return 0
	}
	v, err := binary.ReadUvarint(b.r)
	if err != nil {
		b.err = err
		return 0
	}
	return v
}

func (b *binReader) svarint() int64 {
	if b.err != nil {
		return 0
	}
	v, err := binary.ReadVarint(b.r)
	if err != nil {
		b.err = err
		return 0
	}
	return v
}

func (b *binReader) str() string {
	n := int(b.uvarint())
	if b.err != nil || n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(b.r, buf); err != nil {
		b.err = err
		return ""
	}
	return string(buf)
}

// WriteBinary 把索引写成紧凑二进制（外层 gzip）。
func (ix *Index) WriteBinary(path string) (err error) {
	tmp := path + ".tmp"
	f, cerr := os.Create(tmp)
	if cerr != nil {
		return cerr
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()

	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	bw := &binWriter{w: bufio.NewWriterSize(zw, 1<<20)}
	if _, err = bw.w.WriteString(binMagic); err != nil {
		return err
	}

	// 先摊平出一份只读视图：写盘期间索引可能被并发写入，
	// 摊平后本次写出的就是某一瞬间的一致状态（不会撕裂到半新半旧）。
	v := ix.View()

	// ① 地址表（docLen 的键就是全部文档地址）
	addrs := make([]string, 0, len(v.DocLen))
	addrIdx := make(map[string]int, len(v.DocLen))
	for a := range v.DocLen {
		addrIdx[a] = len(addrs)
		addrs = append(addrs, a)
	}
	bw.uvarint(uint64(len(addrs)))
	for _, a := range addrs {
		bw.str(a)
	}

	// ② 词表
	terms := make([]string, 0, len(v.Postings))
	for t := range v.Postings {
		terms = append(terms, string(t))
	}
	bw.uvarint(uint64(len(terms)))
	for _, t := range terms {
		bw.str(t)
	}

	// ③ 每个文档的长度/字节数/时间
	for _, a := range addrs {
		bw.uvarint(uint64(v.DocLen[a]))
		bw.uvarint(uint64(v.DocSize[a]))
		bw.svarint(v.DocTime[a])
	}

	// ④ 倒排：词 → (地址下标, 词频)
	for _, t := range terms {
		docs := v.Postings[Token(t)]
		bw.uvarint(uint64(len(docs)))
		for a, tf := range docs {
			idx, ok := addrIdx[a]
			if !ok {
				continue // 索引内部不一致：跳过而不是写出坏数据
			}
			bw.uvarint(uint64(idx))
			bw.uvarint(uint64(tf))
		}
	}

	// ⑤ 每篇贡献过的词（精确撤销用）
	termIdx := make(map[string]int, len(terms))
	for i, t := range terms {
		termIdx[t] = i
	}
	for _, a := range addrs {
		toks := v.DocTokens[a]
		bw.uvarint(uint64(len(toks)))
		for _, tk := range toks {
			if i, ok := termIdx[string(tk)]; ok {
				bw.uvarint(uint64(i))
			} else {
				bw.uvarint(0)
			}
		}
	}

	if bw.err != nil {
		return bw.err
	}
	if err = bw.w.Flush(); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadBinary 读回紧凑二进制格式的索引。
func ReadBinary(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, zerr := gzip.NewReader(f)
	if zerr != nil {
		return nil, fmt.Errorf("snapshot is not gzip: %w", zerr)
	}
	defer zr.Close()
	br := &binReader{r: bufio.NewReaderSize(zr, 1<<20)}

	magic := make([]byte, len(binMagic))
	if _, err := io.ReadFull(br.r, magic); err != nil {
		return nil, fmt.Errorf("read magic: %w", err)
	}
	if string(magic) != binMagic {
		return nil, fmt.Errorf("bad snapshot magic %q", magic)
	}

	nAddr := int(br.uvarint())
	addrs := make([]string, nAddr)
	for i := range addrs {
		addrs[i] = br.str()
	}
	nTerm := int(br.uvarint())
	terms := make([]string, nTerm)
	for i := range terms {
		terms[i] = br.str()
	}
	if br.err != nil {
		return nil, br.err
	}

	ix := New()
	ix.totalDocs.Store(int64(nAddr))
	var sumLen int64
	for _, a := range addrs {
		n := int(br.uvarint())
		ix.docLen.Store(a, n)
		sumLen += int64(n)
		ix.docSize.Store(a, int(br.uvarint()))
		ix.docTime.Store(a, br.svarint())
	}
	ix.sumDocLen.Store(sumLen)
	if br.err != nil {
		return nil, br.err
	}

	for _, t := range terms {
		n := int(br.uvarint())
		if br.err != nil {
			return nil, br.err
		}
		m := make(map[string]int, n)
		for i := 0; i < n; i++ {
			ai := int(br.uvarint())
			tf := int(br.uvarint())
			if br.err != nil {
				return nil, br.err
			}
			if ai >= 0 && ai < len(addrs) {
				m[addrs[ai]] = tf
			}
		}
		ps := newPostingSlot()
		entries := make([]postingEntry, 0, len(m))
		for addr, tf := range m {
			entries = append(entries, postingEntry{Addr: addr, TF: tf})
		}
		ps.list.Store(&postingList{entries: entries})
		ix.postings.Store(Token(t), ps)
	}

	for _, a := range addrs {
		n := int(br.uvarint())
		if br.err != nil {
			return nil, br.err
		}
		toks := make([]Token, 0, n)
		for i := 0; i < n; i++ {
			ti := int(br.uvarint())
			if br.err != nil {
				return nil, br.err
			}
			if ti >= 0 && ti < len(terms) {
				toks = append(toks, Token(terms[ti]))
			}
		}
		ix.docTokens.Store(a, toks)
	}
	if br.err != nil {
		return nil, br.err
	}
	return ix, nil
}

// SnapshotPath 返回索引快照在目录里的文件名。
func SnapshotPath(dir string) string {
	return filepath.Join(dir, "index-data.bin")
}

// Snapshot 复制一份索引的只读切片数据，用于在锁外安全写盘。
//
// 为什么必须复制：直接传 *Index 给 WriteBinary，写盘期间
// （几十毫秒到几秒，视数据量和 I/O）如果持有 RLock，写入就会被卡住；
// 如果不持锁，并发写入触发 map 迭代写，runtime 直接杀进程。
// 复制只遍历 map 取值，耗时是微秒量级，在持 RLock 的瞬间完成。
type Snapshot struct {
	Addrs    []string
	Terms    []string
	DocLen   []int
	DocSize  []int
	DocTime  []int64
	Postings [][]PostingEntry
	Tokens   [][]int
}

type PostingEntry struct {
	AddrIdx int
	TF      int
}

// TakeSnapshot 复制出一份只读结构。索引自身无锁，可以直接取。
func (ix *Index) TakeSnapshot() *Snapshot {
	v := ix.View()
	s := &Snapshot{
		Addrs:   make([]string, 0, len(v.DocLen)),
		Terms:   make([]string, 0, len(v.Postings)),
		DocLen:  make([]int, 0, len(v.DocLen)),
		DocSize: make([]int, 0, len(v.DocSize)),
		DocTime: make([]int64, 0, len(v.DocTime)),
	}
	addrIdx := make(map[string]int, len(v.DocLen))
	for a := range v.DocLen {
		addrIdx[a] = len(s.Addrs)
		s.Addrs = append(s.Addrs, a)
		s.DocLen = append(s.DocLen, v.DocLen[a])
		s.DocSize = append(s.DocSize, v.DocSize[a])
		s.DocTime = append(s.DocTime, v.DocTime[a])
	}

	termIdx := make(map[string]int, len(v.Postings))
	for t := range v.Postings {
		termIdx[string(t)] = len(s.Terms)
		s.Terms = append(s.Terms, string(t))
	}

	s.Postings = make([][]PostingEntry, len(s.Terms))
	for i, t := range s.Terms {
		docs := v.Postings[Token(t)]
		entries := make([]PostingEntry, 0, len(docs))
		for a, tf := range docs {
			if idx, ok := addrIdx[a]; ok {
				entries = append(entries, PostingEntry{AddrIdx: idx, TF: tf})
			}
		}
		s.Postings[i] = entries
	}

	s.Tokens = make([][]int, len(s.Addrs))
	for i, a := range s.Addrs {
		toks := v.DocTokens[a]
		indices := make([]int, 0, len(toks))
		for _, tk := range toks {
			if ti, ok := termIdx[string(tk)]; ok {
				indices = append(indices, ti)
			}
		}
		s.Tokens[i] = indices
	}
	return s
}

// WriteSnapshot 把快照写盘（纯 I/O，不碰 *Index，无需持任何锁）。
func WriteSnapshot(s *Snapshot, path string) (err error) {
	tmp := path + ".tmp"
	f, cerr := os.Create(tmp)
	if cerr != nil {
		return cerr
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()

	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	bw := &binWriter{w: bufio.NewWriterSize(zw, 1<<20)}
	if _, err = bw.w.WriteString(binMagic); err != nil {
		return err
	}

	// ① 地址表
	bw.uvarint(uint64(len(s.Addrs)))
	for _, a := range s.Addrs {
		bw.str(a)
	}

	// ② 词表
	bw.uvarint(uint64(len(s.Terms)))
	for _, t := range s.Terms {
		bw.str(t)
	}

	// ③ 文档元数据
	for i := range s.Addrs {
		bw.uvarint(uint64(s.DocLen[i]))
		bw.uvarint(uint64(s.DocSize[i]))
		bw.svarint(s.DocTime[i])
	}

	// ④ 倒排
	for _, entries := range s.Postings {
		bw.uvarint(uint64(len(entries)))
		for _, e := range entries {
			bw.uvarint(uint64(e.AddrIdx))
			bw.uvarint(uint64(e.TF))
		}
	}

	// ⑤ 每篇贡献的词
	for _, toks := range s.Tokens {
		bw.uvarint(uint64(len(toks)))
		for _, ti := range toks {
			bw.uvarint(uint64(ti))
		}
	}

	if bw.err != nil {
		return bw.err
	}
	if err = bw.w.Flush(); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
