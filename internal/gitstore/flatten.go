package gitstore

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
)

// 树 → 命令流 的桥。
//
// 设计见 internal/server/oplog.go：命令流是跨机器一致性的唯一内部抽象，
// 历史层的「整树同步」只是其中一种批量命令。这里提供两个方向的原语：
//
//	FlattenBranch  一棵已提交的树 → 一批 (path, mode, sha) 条目
//	ReadPathSHA    读某路径当前指向的 blob（脏层优先）
//
// 两者都不产生新对象、不改 ref：它们只是把「树」翻译成「命令」。

// FlatEntry 是摊平后的一条树条目。
type FlatEntry struct {
	Path string
	Mode Mode
	SHA  string
}

// FlattenBranch 把一条分支**当前生效的整棵树**摊平成条目列表。
//
// 「当前生效」= 已提交的树叠加未提交的脏层。这一点很关键：整树同步
// 若只导已提交部分，对端拿到的就不是你眼前这棵树——你刚写的东西
// 会凭空消失，而两边都不报错。
//
// 返回按路径排序，保证同一棵树在任何一台机器上导出结果完全一致。
func (r *Repo) FlattenBranch(ref string) ([]FlatEntry, error) {
	if err := r.syncDirtyWriter(); err != nil {
		return nil, fmt.Errorf("process dirty write queue: %w", err)
	}
	normRef := normalizeRef(ref)

	// ① 干净层：已提交的整棵树。
	base := map[string]Entry{}
	if c, err := r.commitOf(normRef); err == nil && c.Tree != "" {
		if err := r.loadTree(c.Tree, "", base); err != nil {
			return nil, err
		}
	}

	// ② 脏层叠加：set 覆盖，del 摘除。
	//
	// 复用 DirtyChanges（它已经处理了目录层级与排序），不另写一份
	// 遍历逻辑——两份「脏层怎么读」迟早会分叉。
	changes, err := r.DirtyChanges(normRef)
	if err != nil {
		return nil, err
	}
	for _, ch := range changes {
		if ch.Deleted {
			delete(base, ch.Path)
			continue
		}
		e := base[ch.Path]
		e.Path = ch.Path
		e.Mode = ch.Mode
		e.Hash = ch.SHA
		base[ch.Path] = e
	}

	out := make([]FlatEntry, 0, len(base))
	for _, e := range base {
		// 子树条目（mode 40000）不是文件，不进命令流：
		// loadTree 已经把它们展开了，这里只兜底。
		if e.Mode == "40000" || e.Hash == "" {
			continue
		}
		out = append(out, FlatEntry{Path: e.Path, Mode: e.Mode, SHA: e.Hash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ReadPathSHA 报告某条路径当前指向的 blob 哈希与模式。
//
// 第二个返回值表示路径是否存在。脏层里的墓碑算「不存在」——
// 这与 Read 的行为一致：被删的内容读不到，也就不该被同步出去。
func (r *Repo) ReadPathSHA(ref, path string) (FlatEntry, bool, error) {
	if err := r.syncDirtyWriter(); err != nil {
		return FlatEntry{}, false, err
	}
	normRef := normalizeRef(ref)
	dir, name := splitDirName(path)

	unlockDirty := r.rlockDirty()
	n, err := r.loadDirtyNode(normRef, dir)
	unlockDirty()
	if err != nil {
		return FlatEntry{}, false, err
	}
	if n != nil {
		if n.del[name] {
			return FlatEntry{}, false, nil
		}
		if e, ok := n.set[name]; ok {
			return FlatEntry{Path: path, Mode: e.Mode, SHA: e.SHA}, true, nil
		}
	}

	c, err := r.commitOf(normRef)
	if err != nil {
		return FlatEntry{}, false, err
	}
	if c.Tree == "" {
		return FlatEntry{}, false, nil
	}
	sha, err := r.findInTree(c.Tree, "", path)
	if err != nil || sha == "" {
		return FlatEntry{}, false, nil
	}
	return FlatEntry{Path: path, Mode: ModeContent, SHA: sha}, true, nil
}

// HashBlob 计算一段正文作为 Git blob 的内容寻址哈希。
//
// 与 applyWorkBlob 的验真用的是同一条规则（"blob <len>\x00" 前缀），
// 集中在这里，免得两处各写一遍迟早对不上。
func HashBlob(body []byte) string {
	sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(body))), body...))
	return hex.EncodeToString(sum[:])
}
