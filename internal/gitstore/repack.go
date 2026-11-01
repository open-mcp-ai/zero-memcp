package gitstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
)

// 这一层自己打包：把松散对象合并成一个 packfile 并写出 .idx v2。
//
// 为什么不用 git gc：服务不该依赖机器上装着 git。而对象数一旦上万，
// 几万个松散文件会让每次读取都要 open 一次——所以打包不是可选项，
// 是「用久了还能用」的前提。
//
// 顺序必须是 pack → idx → 删松散对象：
//   - pack 写完但 idx 没写的窗口里，读路径只认 idx + 松散对象，
//     对象仍然从松散副本读得到；
//   - idx 一旦存在，对象同时有两份，重复但不矛盾；
//   - 最后才删松散副本。任何一步中断都不会让对象消失。

// RepackResult 报告一次打包的结果。
type RepackResult struct {
	Objects  int
	PackName string
	Removed  int
}

// maxRepackBytes 限制一次打包读进内存的总量。
const maxRepackBytes = 1 << 30

// Repack 把仓库里的全部对象打成一个 pack。
func (r *Repo) Repack() (RepackResult, error) {
	shas, err := r.allObjectSHAs()
	if err != nil {
		return RepackResult{}, err
	}
	if len(shas) == 0 {
		return RepackResult{Objects: 0}, nil
	}
	sort.Strings(shas)

	entries := make([]packEntry, 0, len(shas))

	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, uint32(len(shas)))

	var total int
	for _, sha := range shas {
		typ, body, err := r.readObject(sha)
		if err != nil {
			return RepackResult{}, fmt.Errorf("读取对象 %s 失败: %w", sha, err)
		}
		total += len(body) + 32
		if total > maxRepackBytes {
			return RepackResult{}, fmt.Errorf("对象总量超过 %d 字节上限", maxRepackBytes)
		}
		start := pack.Len()
		if err := writePackObject(&pack, typ, body); err != nil {
			return RepackResult{}, err
		}
		raw, _ := hex.DecodeString(sha)
		var sum [20]byte
		copy(sum[:], raw)
		entries = append(entries, packEntry{
			sha:    sum,
			offset: int64(start),
			crc:    crc32.ChecksumIEEE(pack.Bytes()[start:pack.Len()]),
		})
	}

	packSum := sha1.Sum(pack.Bytes())
	full := append(pack.Bytes(), packSum[:]...)
	packName := "pack-" + hex.EncodeToString(packSum[:])

	packDir := filepath.Join(r.Path, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return RepackResult{}, err
	}
	packPath := filepath.Join(packDir, packName+".pack")
	if err := writeFileAtomic(packPath, full, 0o444); err != nil {
		return RepackResult{}, err
	}

	idx, err := buildIdxV2(entries, packSum)
	if err != nil {
		_ = os.Remove(packPath)
		return RepackResult{}, err
	}
	if err := writeFileAtomic(filepath.Join(packDir, packName+".idx"), idx, 0o444); err != nil {
		_ = os.Remove(packPath)
		return RepackResult{}, err
	}
	r.InvalidatePackCache()

	// 校验：索引能被解析，且对象确实读得到。宁可留下重复的松散副本，
	// 也不要在索引写坏的情况下删掉唯一能读的那份。
	idxInfo, err := parseIdx(filepath.Join(packDir, packName+".idx"))
	if err != nil {
		return RepackResult{}, fmt.Errorf("新索引无法解析: %w", err)
	}
	if len(idxInfo.shas) != len(shas) {
		return RepackResult{}, fmt.Errorf("新索引条目数 %d 与对象数 %d 不符", len(idxInfo.shas), len(shas))
	}

	removed := 0
	for _, sha := range shas {
		path := r.objectPath(sha)
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return RepackResult{Objects: len(shas), PackName: packName, Removed: removed}, err
		}
		removed++
	}
	return RepackResult{Objects: len(shas), PackName: packName, Removed: removed}, nil
}

// AllObjectSHAs 收集松散对象与既有 pack 里的全部对象 SHA。
//
// 导出是为了让调用方能验证「某个操作没有偷偷复制对象」——
// fork 的承诺是零字节，那必须能被数出来。
func (r *Repo) AllObjectSHAs() ([]string, error) { return r.allObjectSHAs() }

// allObjectSHAs 收集松散对象与既有 pack 里的全部对象 SHA。
func (r *Repo) allObjectSHAs() ([]string, error) {
	seen := map[string]bool{}

	objDir := filepath.Join(r.Path, "objects")
	fans, err := os.ReadDir(objDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, fan := range fans {
		if !fan.IsDir() || len(fan.Name()) != 2 || fan.Name() == "pack" || fan.Name() == "info" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(objDir, fan.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || len(f.Name()) != 38 {
				continue
			}
			seen[fan.Name()+f.Name()] = true
		}
	}

	packs, err := r.loadPacks()
	if err != nil {
		return nil, err
	}
	for _, p := range packs {
		for _, raw := range p.shas {
			seen[hex.EncodeToString(raw[:])] = true
		}
	}

	out := make([]string, 0, len(seen))
	for sha := range seen {
		out = append(out, sha)
	}
	return out, nil
}

// writePackObject 写一个非 delta 对象条目。
//
// 不打 delta：delta 链要自己算窗口和基对象，算错就是读不出来；
// zlib 已经把文本压得很小，而「读得出来」比「再小一点」重要。
func writePackObject(buf *bytes.Buffer, typ string, body []byte) error {
	var code byte
	switch typ {
	case "commit":
		code = 1
	case "tree":
		code = 2
	case "blob":
		code = 3
	case "tag":
		code = 4
	default:
		return fmt.Errorf("未知对象类型 %q", typ)
	}
	size := len(body)
	head := []byte{code<<4 | byte(size&0x0f)}
	size >>= 4
	for size > 0 {
		head[len(head)-1] |= 0x80
		head = append(head, byte(size&0x7f))
		size >>= 7
	}
	buf.Write(head)

	zw := zlib.NewWriter(buf)
	if _, err := zw.Write(body); err != nil {
		return err
	}
	return zw.Close()
}

// packEntry 是索引里的一条记录。
type packEntry struct {
	sha    [20]byte
	offset int64
	crc    uint32
}

// buildIdxV2 生成 pack 索引 v2。
//
// v2 的布局：magic、版本、256 项 fanout、按 SHA 排序的 20 字节对象名、
// 每项的 CRC32、每项的 32 位偏移（超过 2GB 的走大偏移表）、
// pack 校验和、索引自身校验和。
// fanout 是「首字节 <= i 的对象总数」，读的时候靠它把二分范围缩到 1/256。
func buildIdxV2(entries []packEntry, packSum [20]byte) ([]byte, error) {
	sorted := make([]packEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].sha[:], sorted[j].sha[:]) < 0
	})

	var buf bytes.Buffer
	buf.Write([]byte{0xff, 't', 'O', 'c'})
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))

	var fanout [256]uint32
	for _, e := range sorted {
		fanout[e.sha[0]]++
	}
	var running uint32
	for i := 0; i < 256; i++ {
		running += fanout[i]
		_ = binary.Write(&buf, binary.BigEndian, running)
	}

	for _, e := range sorted {
		buf.Write(e.sha[:])
	}
	for _, e := range sorted {
		_ = binary.Write(&buf, binary.BigEndian, e.crc)
	}

	var large []uint64
	for _, e := range sorted {
		if e.offset < 0x80000000 {
			_ = binary.Write(&buf, binary.BigEndian, uint32(e.offset))
			continue
		}
		_ = binary.Write(&buf, binary.BigEndian, uint32(0x80000000|len(large)))
		large = append(large, uint64(e.offset))
	}
	for _, off := range large {
		_ = binary.Write(&buf, binary.BigEndian, off)
	}

	buf.Write(packSum[:])
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}

// writeFileAtomic 写文件：临时文件 + fsync + rename。
// 目录也要 fsync，否则 rename 本身可能还在页缓存里。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir 在 Unix 上 fsync 目录；Windows 上目录不能 fsync，退化为空操作。
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
