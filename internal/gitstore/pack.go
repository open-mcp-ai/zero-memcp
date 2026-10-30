package gitstore

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// 这一层读 packfile。
//
// 为什么必须有它：松散对象只是 Git 的「未打包」形态。任何一个克隆来的
// 仓库、任何一个跑过 gc 的仓库，对象都在 pack 里（还带增量压缩）。
// 只认松散对象的读取器会在 gc 之后突然瞎掉——而那时服务已经跑了几天，
// 现象是「记忆凭空消失」，没有任何报错指向真正的病因。
//
// pack 的两种 delta 都要支持：
//   OFS_DELTA  基对象在同一个 pack 内，偏移是相对当前对象的负数
//   REF_DELTA  基对象用 20 字节 SHA 指定，可能在任何地方（另一个 pack）

// packIdx 是一个 .idx 索引。
type packIdx struct {
	packPath string
	// 按 SHA 排序，供二分查找
	shas [][20]byte
	// 与 shas 平行的偏移量
	offsets []int64
}

var (
	packCacheMu sync.Mutex
	packCache   = map[string][]*packIdx{} // 仓库路径 -> 索引列表
)

// loadPacks 读出并缓存一个仓库的全部 pack 索引。
func (r *Repo) loadPacks() ([]*packIdx, error) {
	packCacheMu.Lock()
	defer packCacheMu.Unlock()

	if idxs, ok := packCache[r.Path]; ok {
		return idxs, nil
	}

	packDir := filepath.Join(r.Path, "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		if os.IsNotExist(err) {
			packCache[r.Path] = nil
			return nil, nil
		}
		return nil, err
	}

	var idxs []*packIdx
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".idx" {
			continue
		}
		idx, err := parseIdx(filepath.Join(packDir, e.Name()))
		if err != nil {
			// 一个坏索引不该让整个仓库读不了：跳过它，剩下的照读。
			continue
		}
		idxs = append(idxs, idx)
	}
	packCache[r.Path] = idxs
	return idxs, nil
}

// parseIdx 解析 v2 格式的 .idx。
func parseIdx(path string) (*packIdx, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 8+256*4+40 {
		return nil, fmt.Errorf("%s 太短，不像 idx", path)
	}
	if !bytes.Equal(b[:4], []byte{0xff, 0x74, 0x4f, 0x63}) {
		return nil, fmt.Errorf("%s 不是 v2 索引", path)
	}
	if v := binary.BigEndian.Uint32(b[4:8]); v != 2 {
		return nil, fmt.Errorf("%s 索引版本 %d 不支持", path, v)
	}

	fanout := b[8 : 8+256*4]
	count := int(binary.BigEndian.Uint32(fanout[255*4:]))
	if count <= 0 || count > 1<<24 {
		return nil, fmt.Errorf("%s 对象数异常: %d", path, count)
	}

	shaStart := 8 + 256*4
	shaEnd := shaStart + count*20
	crcEnd := shaEnd + count*4
	offEnd := crcEnd + count*4
	if len(b) < offEnd {
		return nil, fmt.Errorf("%s 被截断", path)
	}

	idx := &packIdx{
		packPath: path[:len(path)-len(".idx")] + ".pack",
		shas:     make([][20]byte, count),
		offsets:  make([]int64, count),
	}
	for i := 0; i < count; i++ {
		copy(idx.shas[i][:], b[shaStart+i*20:shaStart+i*20+20])
	}

	// 偏移表：最高位为 1 表示真实值在后面的 8 字节大偏移区
	large := b[offEnd:]
	for i := 0; i < count; i++ {
		v := binary.BigEndian.Uint32(b[crcEnd+i*4 : crcEnd+i*4+4])
		if v&0x80000000 != 0 {
			li := int(v & 0x7fffffff)
			if len(large) < (li+1)*8 {
				return nil, fmt.Errorf("%s 大偏移越界", path)
			}
			idx.offsets[i] = int64(binary.BigEndian.Uint64(large[li*8 : li*8+8]))
		} else {
			idx.offsets[i] = int64(v)
		}
	}
	return idx, nil
}

// find 在索引里二分查找一个 SHA，返回 pack 内偏移。
func (p *packIdx) find(sha [20]byte) (int64, bool) {
	i := sort.Search(len(p.shas), func(i int) bool {
		return bytes.Compare(p.shas[i][:], sha[:]) >= 0
	})
	if i < len(p.shas) && p.shas[i] == sha {
		return p.offsets[i], true
	}
	return 0, false
}

// readPacked 在 pack 里找一个对象，必要时递归解开 delta 链。
func (r *Repo) readPacked(sha string) (string, []byte, error) {
	raw, err := decodeSHA(sha)
	if err != nil {
		return "", nil, err
	}
	idxs, err := r.loadPacks()
	if err != nil {
		return "", nil, err
	}
	for _, idx := range idxs {
		off, ok := idx.find(raw)
		if !ok {
			continue
		}
		return r.readAt(idx, off, 0)
	}
	return "", nil, fmt.Errorf("对象 %s 不在松散库也不在任何 pack 里", sha)
}

// readAt 从某个 pack 偏移处读对象，depth 防止 delta 链成环。
func (r *Repo) readAt(idx *packIdx, off int64, depth int) (string, []byte, error) {
	if depth > 64 {
		return "", nil, fmt.Errorf("delta 链过深（可能成环）")
	}
	f, err := os.Open(idx.packPath)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()

	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", nil, err
	}
	br := newByteReader(f)

	typ, size, err := readObjHeader(br)
	if err != nil {
		return "", nil, err
	}

	switch typ {
	case 1, 2, 3, 4:
		body, err := readZlib(br, size)
		if err != nil {
			return "", nil, err
		}
		return objTypeName(typ), body, nil

	case 6: // OFS_DELTA
		rel, err := br.readVarintOffset()
		if err != nil {
			return "", nil, err
		}
		baseOff := off - rel
		_, base, err := r.readAt(idx, baseOff, depth+1)
		if err != nil {
			return "", nil, fmt.Errorf("读 delta 基对象: %w", err)
		}
		delta, err := readZlib(br, size)
		if err != nil {
			return "", nil, err
		}
		out, err := applyDelta(base, delta)
		if err != nil {
			return "", nil, err
		}
		// delta 的基对象类型决定了结果类型
		bt, _, err := r.readAtType(idx, baseOff)
		if err != nil {
			return "", nil, err
		}
		return bt, out, nil

	case 7: // REF_DELTA
		var baseRaw [20]byte
		if _, err := io.ReadFull(br, baseRaw[:]); err != nil {
			return "", nil, err
		}
		baseSHA := fmt.Sprintf("%x", baseRaw)
		bt, base, err := r.readObject(baseSHA)
		if err != nil {
			return "", nil, fmt.Errorf("读 delta 基对象 %s: %w", baseSHA, err)
		}
		delta, err := readZlib(br, size)
		if err != nil {
			return "", nil, err
		}
		out, err := applyDelta(base, delta)
		if err != nil {
			return "", nil, err
		}
		return bt, out, nil
	}
	return "", nil, fmt.Errorf("未知 pack 对象类型 %d", typ)
}

// readAtType 只求类型，用于让 OFS_DELTA 的结果继承基对象类型。
func (r *Repo) readAtType(idx *packIdx, off int64) (string, int64, error) {
	f, err := os.Open(idx.packPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", 0, err
	}
	typ, size, err := readObjHeader(newByteReader(f))
	if err != nil {
		return "", 0, err
	}
	if typ >= 1 && typ <= 4 {
		return objTypeName(typ), size, nil
	}
	// delta 的基对象也是 delta：继续往上找
	br := newByteReader(f)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", 0, err
	}
	br = newByteReader(f)
	_, _, _ = readObjHeader(br)
	if typ == 6 {
		rel, err := br.readVarintOffset()
		if err != nil {
			return "", 0, err
		}
		return r.readAtType(idx, off-rel)
	}
	if typ == 7 {
		var baseRaw [20]byte
		if _, err := io.ReadFull(br, baseRaw[:]); err != nil {
			return "", 0, err
		}
		baseSHA := fmt.Sprintf("%x", baseRaw)
		idxs, err := r.loadPacks()
		if err != nil {
			return "", 0, err
		}
		for _, i2 := range idxs {
			if o, ok := i2.find(baseRaw); ok {
				return r.readAtType(i2, o)
			}
		}
		_ = baseSHA
	}
	return "", 0, fmt.Errorf("无法确定对象类型")
}

func objTypeName(t int) string {
	switch t {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return "unknown"
}

// byteReader 是带一个小缓冲的顺序读取器——pack 头部的 varint 要逐字节读，
// 直接对 *os.File 做单字节 Read 会产生大量系统调用。
type byteReader struct {
	r   io.Reader
	buf [1]byte
}

func newByteReader(r io.Reader) *byteReader { return &byteReader{r: r} }

func (b *byteReader) Read(p []byte) (int, error) {
	// 先吃掉缓冲区里的那个字节（如果有）
	if b.buf[0] != 0 {
		if len(p) == 0 {
			return 0, nil
		}
		p[0] = b.buf[0]
		b.buf[0] = 0
		n, err := b.r.Read(p[1:])
		return n + 1, err
	}
	return b.r.Read(p)
}

func (b *byteReader) readByte() (byte, error) {
	var one [1]byte
	if _, err := io.ReadFull(b.r, one[:]); err != nil {
		return 0, err
	}
	return one[0], nil
}

// readObjHeader 读 pack 对象头：低 4 位是类型，之后是变长的长度。
func readObjHeader(br *byteReader) (typ int, size int64, err error) {
	c, err := br.readByte()
	if err != nil {
		return 0, 0, err
	}
	typ = int(c>>4) & 0x7
	size = int64(c & 0x0f)
	shift := uint(4)
	for c&0x80 != 0 {
		c, err = br.readByte()
		if err != nil {
			return 0, 0, err
		}
		size |= int64(c&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, fmt.Errorf("对象长度溢出")
		}
	}
	return typ, size, nil
}

// readVarintOffset 读 OFS_DELTA 的相对偏移。
//
// 编码方式和普通 varint 不同：每多读一个字节，前面的值要 +1 再左移。
// 这不是「更省空间」的花招，而是为了让越近的基对象编码越短。
func (b *byteReader) readVarintOffset() (int64, error) {
	c, err := b.readByte()
	if err != nil {
		return 0, err
	}
	off := int64(c & 0x7f)
	for c&0x80 != 0 {
		c, err = b.readByte()
		if err != nil {
			return 0, err
		}
		off = ((off + 1) << 7) | int64(c&0x7f)
	}
	return off, nil
}

// readZlib 读出 zlib 压缩的数据段。
func readZlib(r io.Reader, size int64) ([]byte, error) {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out := make([]byte, size)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, err
	}
	return out, nil
}

// applyDelta 把 delta 应用到基对象上。
//
// delta 由三部分组成：基对象长度、结果长度、一串指令。
// 指令高位为 1 表示「从基对象拷贝一段」，为 0 表示「插入一段字面量」。
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readSize := func() (int, error) {
		var v int
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("delta 长度字段越界")
			}
			c := delta[pos]
			pos++
			v |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return v, nil
			}
		}
	}

	baseSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if baseSize != len(base) {
		return nil, fmt.Errorf("delta 声明的基对象长度 %d 与实际 %d 不符", baseSize, len(base))
	}
	outSize, err := readSize()
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, outSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// 拷贝：低 4 位选偏移字节，接着 3 位选长度字节
			var cpOff, cpLen int
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta 拷贝偏移越界")
					}
					cpOff |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(1<<uint(4+i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta 拷贝长度越界")
					}
					cpLen |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if cpLen == 0 {
				cpLen = 0x10000
			}
			if cpOff < 0 || cpOff+cpLen > len(base) {
				return nil, fmt.Errorf("delta 拷贝区间越界")
			}
			out = append(out, base[cpOff:cpOff+cpLen]...)
			continue
		}
		if op == 0 {
			return nil, fmt.Errorf("delta 里出现保留指令 0")
		}
		// 插入字面量
		n := int(op)
		if pos+n > len(delta) {
			return nil, fmt.Errorf("delta 插入越界")
		}
		out = append(out, delta[pos:pos+n]...)
		pos += n
	}
	if len(out) != outSize {
		return nil, fmt.Errorf("delta 结果长度 %d 与声明 %d 不符", len(out), outSize)
	}
	return out, nil
}

func decodeSHA(sha string) ([20]byte, error) {
	var out [20]byte
	if err := validSHA(sha); err != nil {
		return out, err
	}
	for i := 0; i < 20; i++ {
		hi, ok1 := hexVal(sha[i*2])
		lo, ok2 := hexVal(sha[i*2+1])
		if !ok1 || !ok2 {
			return out, fmt.Errorf("非法 SHA %q", sha)
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// InvalidatePackCache 在仓库新增了 pack 之后清掉缓存。
func (r *Repo) InvalidatePackCache() {
	packCacheMu.Lock()
	delete(packCache, r.Path)
	packCacheMu.Unlock()
}
