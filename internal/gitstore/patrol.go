package gitstore

import (
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// 对象回读巡检（object patrol）。
//
// 要防的故障：writeObject 里那行
//
//	if _, err := r.FS().Stat(dst); err == nil { return sha, nil }
//
// 意思是「文件存在 → 内容完好」。在断电、磁盘满、文件系统掉块之后，
// 文件完全可能长度为零、或者内容被截断，而 Stat 一律说「在」。
// 此后每一次读取都以为拿到了好数据，直到某篇记忆莫名消失。
//
// 所以写入路径保留 Stat 快路径（它省掉 99% 的重复压缩），但补两层：
//
//	① 写后抽检：新落盘的对象按比例回读一次，解码后重算 SHA-1 对拍。
//	② 后台巡边：低速扫描存量对象，发现坏的记进状态并上报，不自动删。
//
// 为什么不「每次写都回读」：那条路径在正常磁盘上永远通过，白付一次
// 解压开销；而真正的坏对象总是少数，抽检 + 巡边足以在它变成事故前
// 被看见。判据是「能不能在一个具体案子上跑通」，不是覆盖率数字好看。

// patrolConfig 控制回读强度。
type patrolConfig struct {
	// WriteSampleEvery 每 N 次新写入回读一次；0 表示关闭。
	WriteSampleEvery int64
	// ScanBatch 是每轮巡边检查的对象数。
	ScanBatch int
	// ScanInterval 是两轮巡边之间的间隔。
	ScanInterval time.Duration
}

var defaultPatrol = patrolConfig{
	WriteSampleEvery: 16,
	ScanBatch:        100,
	ScanInterval:     3 * time.Minute,
}

// ObjectHealth 汇总巡边结果，供 status 展示。
type ObjectHealth struct {
	// Checked 是累计已回读的对象数。
	Checked int64
	// Corrupt 是内容与名字不符的对象数。
	Corrupt int64
	// LastCorrupt 是最近一次发现坏对象的时间与地址（便于定位）。
	LastCorrupt string
	// LastRun 是最近一轮巡边开始时间。
	LastRun time.Time
}

// writeCounter 用于按比例抽检，进程内计数即可。
var writeCounter atomic.Int64

// patchWriteObjectStat 在 writeObject 命中 Stat 快路径时插入抽检。
//
// 它返回 true 表示「这个对象存在但内容坏了」——那种情况下调用方应当
// 覆盖重写，而不是继续信任它。
func (r *Repo) objectLooksTrustworthy(dst, sha string) bool {
	n := writeCounter.Add(1)
	every := r.patrol.WriteSampleEvery
	if every <= 0 || n%every != 0 {
		// 未抽到：沿用快路径。
		return true
	}
	return r.verifyObjectFile(dst, sha) == nil
}

// verifyObjectFile 解码一个松散对象并重算 SHA-1。
func (r *Repo) verifyObjectFile(dst, expectedSHA string) error {
	f, err := r.FS().Open(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return fmt.Errorf("object %s is not readable zlib data: %w", expectedSHA, err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, 1<<30))
	if err != nil {
		return fmt.Errorf("object %s truncated: %w", expectedSHA, err)
	}
	sum := sha1.Sum(raw)
	if got := hex.EncodeToString(sum[:]); got != expectedSHA {
		return fmt.Errorf("object %s content hashes to %s", expectedSHA, got)
	}
	return nil
}

// objectsRoot 返回 objects/ 目录。
func (r *Repo) objectsRoot() string { return filepath.Join(r.Path, "objects") }

// VerifyObject 公开版本：检查一个对象是否内容完好。
func (r *Repo) VerifyObject(sha string) error {
	return r.verifyObjectFile(r.objectPath(sha), sha)
}

// PatrolObjects 扫一批松散对象，返回坏掉的地址。
//
// 只扫松散对象（objects/xx/...）：pack 有自己的校验和，损坏时
// 整个 pack 都不可读，症状完全不同，不该混在一起处理。
func (r *Repo) PatrolObjects(limit int) (checked, corrupt int, bad []string, err error) {
	root := r.objectsRoot()
	entries, rerr := r.FS().ReadDir(root)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, 0, nil, nil
		}
		return 0, 0, nil, rerr
	}
	for _, d := range entries {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		dir := d.Name()
		files, derr := r.FS().ReadDir(filepath.Join(root, dir))
		if derr != nil {
			continue
		}
		for _, f := range files {
			if checked >= limit {
				return checked, corrupt, bad, nil
			}
			name := f.Name()
			if len(name) != 38 {
				continue
			}
			sha := dir + name
			checked++
			if verr := r.VerifyObject(sha); verr != nil {
				corrupt++
				if len(bad) < 10 {
					bad = append(bad, sha)
				}
			}
		}
	}
	return checked, corrupt, bad, nil
}
