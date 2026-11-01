package gitstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// 这一层接收 packfile：把远端送来的一包对象还原成松散对象写进本仓库。
//
// 为什么落成松散对象而不是原样存 pack：
//   - 存 pack 要额外生成 .idx（v2 的 fanout / CRC / 大偏移表），
//     任何一处算错都会让整个仓库读不出对象，而错误在写入时看不出来；
//   - 松散对象走的是已经在用的 writeObject，幂等、原子、断电安全；
//   - 代价是文件多、后续要 gc。而 gc 是显式动作，不是正确性的前提。
//
// delta 必须在这里展开：OFS_DELTA 的基对象在本包内（按偏移），
// REF_DELTA 的基对象可能在别的包或本地。拿不到基对象就报错——
// 猜一个近似值等于把内容写坏，而写坏的内容是不可逆的。

// IngestResult 报告一次接收的结果。
type IngestResult struct {
	// Objects 是包里声明的对象数。
	Objects int
	// Written 是实际写盘的对象数（同一对象重复出现会被内容寻址去重）。
	Written int
}

// maxIngestBytes 是单次接收的上限。
// 超过就报错而不是慢慢吃掉内存：这是个本地服务，不是流水线。
const maxIngestBytes = 512 << 20

// packObj 是 pack 里的一个对象条目。
type packObj struct {
	offset  int
	typ     int // 1 commit, 2 tree, 3 blob, 4 tag, 6 ofs-delta, 7 ref-delta
	data    []byte
	baseOff int
	baseSHA string
}

// IngestPack 解析一个 PACK v2/v3 数据流，把对象写进仓库。
func (r *Repo) IngestPack(data []byte) (IngestResult, error) {
	objs, err := parsePack(data)
	if err != nil {
		return IngestResult{}, err
	}

	result := IngestResult{Objects: len(objs)}
	byOffset := map[int]*packObj{}
	for _, o := range objs {
		byOffset[o.offset] = o
	}
	// memo 记住每个偏移对应对象展开后的 (类型, 内容)，delta 链不必重复展开。
	memo := map[int]packBase{}

	for _, o := range objs {
		base, err := r.resolvePackObj(o, byOffset, memo)
		if err != nil {
			return result, err
		}
		if _, err := r.writeObject(base.typ, base.data); err != nil {
			return result, err
		}
		result.Written++
	}
	return result, nil
}

// packBase 是展开后的对象：类型名 + 内容。
type packBase struct {
	typ  string
	data []byte
}

// resolvePackObj 把可能带 delta 的条目展开成完整内容。
func (r *Repo) resolvePackObj(o *packObj, byOffset map[int]*packObj, memo map[int]packBase) (packBase, error) {
	if m, ok := memo[o.offset]; ok {
		return m, nil
	}
	switch o.typ {
	case 1, 2, 3, 4:
		m := packBase{typ: typeName(o.typ), data: o.data}
		memo[o.offset] = m
		return m, nil
	}

	var base packBase
	var err error
	switch o.typ {
	case 6:
		b, ok := byOffset[o.baseOff]
		if !ok {
			return packBase{}, fmt.Errorf("delta 基对象偏移 %d 不在包内", o.baseOff)
		}
		base, err = r.resolvePackObj(b, byOffset, memo)
		if err != nil {
			return packBase{}, err
		}
	case 7:
		// 基对象可能在本包内（还没展开），也可能早就在本地对象库里。
		if b := findPackObjBySHA(o.baseSHA, byOffset); b != nil {
			base, err = r.resolvePackObj(b, byOffset, memo)
			if err != nil {
				return packBase{}, err
			}
			break
		}
		typ, terr := r.objectType(o.baseSHA)
		if terr != nil {
			return packBase{}, fmt.Errorf("delta 基对象 %s 不在本地，需要更完整的历史: %w", o.baseSHA, terr)
		}
		body, rerr := r.readObjectBody(o.baseSHA)
		if rerr != nil {
			return packBase{}, fmt.Errorf("读取 delta 基对象 %s: %w", o.baseSHA, rerr)
		}
		base = packBase{typ: typ, data: body}
	default:
		return packBase{}, fmt.Errorf("未知的 pack 对象类型 %d", o.typ)
	}

	merged, err := applyDelta(base.data, o.data)
	if err != nil {
		return packBase{}, fmt.Errorf("还原 delta: %w", err)
	}
	m := packBase{typ: base.typ, data: merged}
	memo[o.offset] = m
	return m, nil
}

// findPackObjBySHA 在包内找一个 commit/tree/blob 内容对应的条目。
//
// 只在 REF_DELTA 的基对象落进同一个包时才有用；找不到就回退到本地对象库。
func findPackObjBySHA(sha string, byOffset map[int]*packObj) *packObj {
	for _, o := range byOffset {
		if o.typ < 1 || o.typ > 4 {
			continue
		}
		header := fmt.Sprintf("%s %d\x00", typeName(o.typ), len(o.data))
		sum := sha1.Sum(append([]byte(header), o.data...))
		if hex.EncodeToString(sum[:]) == sha {
			return o
		}
	}
	return nil
}

// parsePack 校验并拆出 pack 里的全部条目。
func parsePack(data []byte) ([]*packObj, error) {
	if len(data) < 32 {
		return nil, errors.New("pack 过短")
	}
	if len(data) > maxIngestBytes {
		return nil, fmt.Errorf("pack 超过 %d 字节上限", maxIngestBytes)
	}
	if string(data[:4]) != "PACK" {
		return nil, errors.New("不是 pack 数据（缺 PACK 头）")
	}
	if version := int(be32(data[4:8])); version != 2 && version != 3 {
		return nil, fmt.Errorf("不支持的 pack 版本 %d", version)
	}
	body := data[:len(data)-20]
	sum := sha1.Sum(body)
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return nil, errors.New("pack 校验和不匹配（传输中被截断或损坏）")
	}
	count := int(be32(data[8:12]))
	if count < 0 || count > maxIngestBytes/16 {
		return nil, fmt.Errorf("pack 对象数异常：%d", count)
	}

	objs := make([]*packObj, 0, count)
	pos := 12
	for i := 0; i < count; i++ {
		if pos >= len(body) {
			return nil, fmt.Errorf("pack 在第 %d 个对象处提前结束", i+1)
		}
		start := pos
		b := body[pos]
		typ := int(b>>4) & 7
		size := int64(b & 0x0f)
		shift := uint(4)
		pos++
		for b&0x80 != 0 {
			if pos >= len(body) || shift > 56 {
				return nil, errors.New("非法的对象头长度编码")
			}
			b = body[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		if size < 0 || size > maxIngestBytes {
			return nil, fmt.Errorf("对象 %d 声明大小异常：%d", i+1, size)
		}
		cur := &packObj{offset: start, typ: typ}

		switch typ {
		case 6:
			if pos >= len(body) {
				return nil, errors.New("delta 头被截断")
			}
			c := uint64(body[pos])
			pos++
			off := c & 0x7f
			for c&0x80 != 0 {
				if pos >= len(body) {
					return nil, errors.New("delta 偏移被截断")
				}
				c = uint64(body[pos])
				pos++
				off = ((off + 1) << 7) | (c & 0x7f)
			}
			if off > uint64(start) {
				return nil, errors.New("delta 基对象偏移越界")
			}
			cur.baseOff = start - int(off)
		case 7:
			if pos+20 > len(body) {
				return nil, errors.New("delta 基对象哈希被截断")
			}
			cur.baseSHA = hex.EncodeToString(body[pos : pos+20])
			pos += 20
		}

		// bytes.Reader 实现了 io.ByteReader，flate 因此不会读过压缩流末尾，
		// 剩余长度就是下一个对象的起点。
		br := bytes.NewReader(body[pos:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("对象 %d 解压失败: %w", i+1, err)
		}
		out, rerr := io.ReadAll(io.LimitReader(zr, size+1))
		cerr := zr.Close()
		if rerr != nil {
			return nil, fmt.Errorf("对象 %d 解压中断: %w", i+1, rerr)
		}
		if cerr != nil {
			return nil, fmt.Errorf("对象 %d 解压收尾失败: %w", i+1, cerr)
		}
		if int64(len(out)) != size {
			return nil, fmt.Errorf("对象 %d 大小不符：声明 %d，实得 %d", i+1, size, len(out))
		}
		pos = len(body) - br.Len()
		cur.data = out
		objs = append(objs, cur)
	}
	if pos != len(body) {
		return nil, fmt.Errorf("pack 尾部有多余 %d 字节", len(body)-pos)
	}
	return objs, nil
}

func typeName(t int) string {
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
	return "blob"
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// objectType 返回已有对象的类型名。
func (r *Repo) objectType(sha string) (string, error) {
	typ, _, err := r.readObject(sha)
	return typ, err
}

// readObjectBody 返回已有对象的内容（不含头）。
func (r *Repo) readObjectBody(sha string) ([]byte, error) {
	_, body, err := r.readObject(sha)
	return body, err
}
