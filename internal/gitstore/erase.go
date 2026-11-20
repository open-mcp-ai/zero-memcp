package gitstore

import (
	"fmt"
	"os"
	"sort"
	"time"
)

// 世界树改写（World-Tree Rewrite）：blob 级彻底抹除与替换。
//
// 为什么针对 blob 而不是针对 path：
//
//	在内容寻址存储里，同一份记忆可能被移动、改名、或者在历史里
//	出现过五次。只删「当前路径」，只是把书从目录里划掉，书页还在
//	后面躺着——git log 一翻就出来。抹除必须以 blob SHA 为单位，
//	让这个对象从整棵历史树的所有分支节点里一起消失。
//
// 为什么允许在封闭圈内重放整条链：
//
//	这是用户明确授权的契约——「数据的销毁权高于历史的连续性」，
//	但只对自己与自己的 Git 仓库承诺（外部托管做不到）。
//	本地全链重写后，对等体走强推覆盖；并且每改写一次就留下一个
//	「不可见审计标记（X）」：什么时间、谁、动了哪条分支。内容消失，
//	「曾经改写过」这件事本身不可消除。

// EraseReport 记录一次世界树改写的事实。
type EraseReport struct {
	TargetBlob       string    `json:"target_blob"`
	ReplacementBlob  string    `json:"replacement_blob,omitempty"` // 空表示彻底删除
	Branch           string    `json:"branch"`
	OldHead          string    `json:"old_head"`
	NewHead          string    `json:"new_head"`
	RewrittenCommits int       `json:"rewritten_commits"`
	TouchedPaths     []string  `json:"touched_paths"`
	DirtyCleared     int       `json:"dirty_cleared"`
	LooseRemoved     bool      `json:"loose_removed"`
	InPack           bool      `json:"in_pack"` // 若为真，旧 blob 还在 pack 里，需 Repack 物理清盘
	At               time.Time `json:"at"`
}

// EraseBlob 在指定分支的整条历史与工作层中抹除（replacement == nil）
// 或替换（replacement != nil）一个目标 blob。
//
// 注意：它重写 commit 指针，因此调用方必须持有分支写权限，并且在
// 完成后需要通知对等节点（强制同步 + 重建内存索引）。
func (r *Repo) EraseBlob(ref, targetSHA string, replacement []byte) (EraseReport, error) {
	normRef := normalizeRef(ref)
	if err := ValidateBranch(normRef); err != nil {
		return EraseReport{}, err
	}
	if err := validSHA(targetSHA); err != nil {
		return EraseReport{}, fmt.Errorf("target blob %q: %w", targetSHA, err)
	}
	if err := r.syncDirtyWriter(); err != nil {
		return EraseReport{}, err
	}

	rep := EraseReport{
		TargetBlob: targetSHA,
		Branch:     normRef,
		At:         time.Now(),
	}

	var newBlobSHA string
	if replacement != nil {
		sha, err := r.writeObject("blob", replacement)
		if err != nil {
			return rep, err
		}
		newBlobSHA = sha
		rep.ReplacementBlob = sha
	}

	// ── 1. 先清理工作层（脏层）里还挂着的引用 ──
	dirtyCleared, err := r.rewriteDirtyBlob(normRef, targetSHA, newBlobSHA)
	if err != nil {
		return rep, fmt.Errorf("rewrite dirty layer: %w", err)
	}
	rep.DirtyCleared = dirtyCleared

	// ── 2. 从分支 HEAD 往前收集整条 commit 链（拓扑序：最老在前）──
	oldHead, err := r.Resolve(normRef)
	if err != nil {
		return rep, err
	}
	rep.OldHead = oldHead
	rep.NewHead = oldHead

	touchedSet := map[string]bool{}
	if oldHead != "" {
		order, commits, err := r.collectCommitChain(oldHead)
		if err != nil {
			return rep, fmt.Errorf("walk commit history: %w", err)
		}

		// oldSHA -> newSHA 映射。没受影响且父母没变的 commit 原封不动，
		// 一旦某个祖先变了，它后面的每个子孙都必须重算（parent 变了）。
		mapped := make(map[string]string, len(order))
		for _, oldSHA := range order {
			c := commits[oldSHA]
			parentsChanged := false
			newParents := make([]string, len(c.Parents))
			for i, p := range c.Parents {
				if np, ok := mapped[p]; ok && np != p {
					newParents[i] = np
					parentsChanged = true
				} else {
					newParents[i] = p
				}
			}
			newTree, treeChanged, paths, terr := r.rewriteTreeBlob(c.Tree, targetSHA, newBlobSHA)
			if terr != nil {
				return rep, fmt.Errorf("rewrite tree at %s: %w", oldSHA, terr)
			}
			for _, p := range paths {
				touchedSet[p] = true
			}
			if !treeChanged && !parentsChanged {
				mapped[oldSHA] = oldSHA
				continue
			}
			newSHA, werr := r.writeCommitPreservingAuthor(newTree, newParents, c)
			if werr != nil {
				return rep, fmt.Errorf("rewrite commit %s: %w", oldSHA, werr)
			}
			mapped[oldSHA] = newSHA
			rep.RewrittenCommits++
		}
		if nh, ok := mapped[oldHead]; ok && nh != oldHead {
			fullRef := "refs/heads/" + normRef
			if err := r.SetRef(fullRef, nh); err != nil {
				return rep, fmt.Errorf("advance ref %s: %w", fullRef, err)
			}
			rep.NewHead = nh
		}
	}

	for p := range touchedSet {
		rep.TouchedPaths = append(rep.TouchedPaths, p)
	}
	sort.Strings(rep.TouchedPaths)

	// ── 3. 物理销毁松散对象（如果在 pack 里则标记需要 repack 清盘）──
	loose := r.objectPath(targetSHA)
	if _, err := r.FS().Stat(loose); err == nil {
		_ = os.Chmod(loose, 0o644)
		if rerr := r.FS().Remove(loose); rerr == nil {
			rep.LooseRemoved = true
		}
	}
	if r.HasObject(targetSHA) {
		rep.InPack = true
	}
	return rep, nil
}

// collectCommitChain 从 head 往回遍历所有可达 commit，返回从老到新的顺序。
func (r *Repo) collectCommitChain(head string) ([]string, map[string]commitObj, error) {
	seen := map[string]bool{}
	commits := map[string]commitObj{}
	var postOrder []string

	var visit func(sha string) error
	visit = func(sha string) error {
		if sha == "" || seen[sha] {
			return nil
		}
		seen[sha] = true
		typ, body, err := r.readObject(sha)
		if err != nil {
			return err
		}
		if typ != "commit" {
			return fmt.Errorf("%s is %s, not commit", sha, typ)
		}
		c := parseCommit(body)
		commits[sha] = c
		for _, p := range c.Parents {
			if err := visit(p); err != nil {
				return err
			}
		}
		postOrder = append(postOrder, sha)
		return nil
	}
	if err := visit(head); err != nil {
		return nil, nil, err
	}
	return postOrder, commits, nil
}

// rewriteTreeBlob 在一棵干净树里删掉或替换 targetSHA。
func (r *Repo) rewriteTreeBlob(treeSHA, targetSHA, newBlobSHA string) (string, bool, []string, error) {
	if treeSHA == "" {
		return "", false, nil, nil
	}
	paths := map[string]Entry{}
	if err := r.loadTree(treeSHA, "", paths); err != nil {
		return "", false, nil, err
	}
	var touched []string
	changed := false
	for p, e := range paths {
		if e.Hash != targetSHA {
			continue
		}
		changed = true
		touched = append(touched, p)
		if newBlobSHA == "" {
			delete(paths, p)
		} else {
			e.Hash = newBlobSHA
			paths[p] = e
		}
	}
	if !changed {
		return treeSHA, false, nil, nil
	}
	newTree, err := r.writeTreeFromPaths(paths)
	if err != nil {
		return "", false, nil, err
	}
	return newTree, true, touched, nil
}

// rewriteDirtyBlob 清理工作层中指向 targetSHA 的条目，以及清除内存缓存。
func (r *Repo) rewriteDirtyBlob(ref, targetSHA, newBlobSHA string) (int, error) {
	changes, err := r.DirtyChanges(ref)
	if err != nil {
		return 0, err
	}
	cleared := 0
	for _, ch := range changes {
		if ch.Deleted || ch.SHA != targetSHA {
			continue
		}
		if newBlobSHA == "" {
			if err := r.ApplyChange(ref, ch.Path, "", "", true); err != nil {
				return cleared, err
			}
		} else {
			if err := r.ApplyChange(ref, ch.Path, ch.Mode, newBlobSHA, false); err != nil {
				return cleared, err
			}
		}
		cleared++
	}
	return cleared, nil
}

// writeCommitPreservingAuthor 重演提交时尽量保住原作者与时间戳，
// 使得未受影响的记忆时间线不会因为一次世界树改写而全部变成「今天」。
func (r *Repo) writeCommitPreservingAuthor(treeSHA string, parents []string, orig commitObj) (string, error) {
	if orig.Author == "" {
		when := orig.When
		if when.IsZero() {
			when = time.Now()
		}
		return r.writeCommit(treeSHA, parents, orig.Message, when)
	}
	var buf []byte
	buf = fmt.Appendf(buf, "tree %s\n", treeSHA)
	for _, p := range parents {
		buf = fmt.Appendf(buf, "parent %s\n", p)
	}
	buf = fmt.Appendf(buf, "author %s\n", orig.Author)
	buf = fmt.Appendf(buf, "committer %s\n\n", orig.Author)
	buf = append(buf, orig.Message...)
	if len(orig.Message) == 0 || orig.Message[len(orig.Message)-1] != '\n' {
		buf = append(buf, '\n')
	}
	return r.writeObject("commit", buf)
}
