package gitstore

import (
	"fmt"
	"sort"
	"strings"
)

// commitForTreeRef 允许树差分游标使用稳定的 commit SHA，或使用分支名。
func (r *Repo) commitForTreeRef(ref string) (commitObj, error) {
	if validSHA(ref) != nil {
		return r.commitOf(ref)
	}
	typ, body, err := r.readObject(ref)
	if err != nil {
		return commitObj{}, err
	}
	if typ != "commit" {
		return commitObj{}, fmt.Errorf("对象 %s 不是 commit", ref)
	}
	return parseCommit(body), nil
}

// TreeAt 返回某个 ref 下指定路径对应的树 SHA。
//
// 路径为空时返回根树。这条是「按树订阅」的地基：比较两棵树在同一
// 路径上的 SHA，就知道那棵子树动没动，不需要展开任何条目。
func (r *Repo) TreeAt(ref, path string) (string, error) {
	c, err := r.commitForTreeRef(ref)
	if err != nil {
		return "", err
	}
	if c.Tree == "" {
		return "", nil
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return c.Tree, nil
	}
	cur := c.Tree
	for _, seg := range strings.Split(path, "/") {
		items, err := r.treeItems(cur)
		if err != nil {
			return "", err
		}
		found := ""
		for _, it := range items {
			if it.name == seg {
				if it.mode != "40000" {
					return "", fmt.Errorf("%s 不是目录", seg)
				}
				found = it.sha
				break
			}
		}
		if found == "" {
			return "", nil
		}
		cur = found
	}
	return cur, nil
}

// treeItems 读一个树对象的条目。
func (r *Repo) treeItems(sha string) ([]treeItem, error) {
	if sha == "" {
		return nil, nil
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return nil, err
	}
	if typ != "tree" {
		return nil, fmt.Errorf("对象 %s 不是 tree", sha)
	}
	return parseTree(body)
}

// SubtreeChanged 报告两棵树在同一路径上的 SHA 是否不同。
//
// 两边都取不到（路径不存在）时算未变化：从无到有会被 Get 判定为
// 不同，从无到无不是事件。
func (r *Repo) SubtreeChanged(oldRef, newRef, path string) (bool, error) {
	oldSHA, err := r.TreeAt(oldRef, path)
	if err != nil {
		return false, err
	}
	newSHA, err := r.TreeAt(newRef, path)
	if err != nil {
		return false, err
	}
	return oldSHA != newSHA, nil
}

// SubtreeDiff 返回某个路径下两棵树之间的路径级变化。
//
// 它递归比较 Merkle 子树 SHA：相同子树 O(1) 短路，只有真正变化的
// 目录才会展开。不能先把两边整棵树 loadTree 成 map 再比较——那样
// 虽然结果正确，却把 Merkle 树的核心优势丢光了。
func (r *Repo) SubtreeDiff(oldRef, newRef, path string) ([]Change, error) {
	oldSHA, err := r.TreeAt(oldRef, path)
	if err != nil {
		return nil, err
	}
	newSHA, err := r.TreeAt(newRef, path)
	if err != nil {
		return nil, err
	}
	if oldSHA == newSHA {
		return nil, nil
	}
	prefix := strings.Trim(path, "/")
	var out []Change
	if err := r.diffTree(oldSHA, newSHA, prefix, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// diffTree 比较两个 tree SHA，并在递归过程中对相同子树做哈希短路。
func (r *Repo) diffTree(oldSHA, newSHA, prefix string, out *[]Change) error {
	if oldSHA == newSHA {
		return nil
	}
	oldItems, err := r.treeItems(oldSHA)
	if err != nil {
		return err
	}
	newItems, err := r.treeItems(newSHA)
	if err != nil {
		return err
	}
	oldByName := make(map[string]treeItem, len(oldItems))
	newByName := make(map[string]treeItem, len(newItems))
	for _, it := range oldItems {
		oldByName[it.name] = it
	}
	for _, it := range newItems {
		newByName[it.name] = it
	}

	seen := make(map[string]bool, len(oldByName)+len(newByName))
	for name, old := range oldByName {
		seen[name] = true
		new, exists := newByName[name]
		p := name
		if prefix != "" {
			p = prefix + "/" + name
		}
		if !exists {
			if old.mode == "40000" {
				if err := r.collectTree(old.sha, p, "deleted", out); err != nil {
					return err
				}
			} else {
				*out = append(*out, Change{Path: p, Kind: "deleted"})
			}
			continue
		}
		if old.mode == "40000" && new.mode == "40000" {
			if err := r.diffTree(old.sha, new.sha, p, out); err != nil {
				return err
			}
			continue
		}
		if old.mode != new.mode || old.sha != new.sha {
			*out = append(*out, Change{Path: p, Kind: "modified"})
		}
	}
	for name, item := range newByName {
		if seen[name] {
			continue
		}
		p := name
		if prefix != "" {
			p = prefix + "/" + name
		}
		if item.mode == "40000" {
			if err := r.collectTree(item.sha, p, "added", out); err != nil {
				return err
			}
		} else {
			*out = append(*out, Change{Path: p, Kind: "added"})
		}
	}
	return nil
}

// collectTree emits leaf changes only when an entire subtree was added/deleted.
func (r *Repo) collectTree(sha, prefix, kind string, out *[]Change) error {
	items, err := r.treeItems(sha)
	if err != nil {
		return err
	}
	for _, it := range items {
		p := prefix + "/" + it.name
		if it.mode == "40000" {
			if err := r.collectTree(it.sha, p, kind, out); err != nil {
				return err
			}
		} else {
			*out = append(*out, Change{Path: p, Kind: kind})
		}
	}
	return nil
}

// TouchedUnder 报告一个提交相对于父提交，在哪些给定路径前缀下发生了变化。
//
// 这是提交落盘后唯一需要回答的问题：「谁关注的那棵树动过」。
// 对每个前缀只做一次 SHA 比较，不做全树遍历。
func (r *Repo) TouchedUnder(commitSHA string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	typ, body, err := r.readObject(commitSHA)
	if err != nil {
		return nil, err
	}
	if typ != "commit" {
		return nil, fmt.Errorf("对象 %s 不是 commit", commitSHA)
	}
	c := parseCommit(body)
	newTree := c.Tree

	// 没有父提交（首次提交）：所有存在的路径都算变化。
	var oldTree string
	if len(c.Parents) > 0 {
		ptype, pbody, perr := r.readObject(c.Parents[0])
		if perr == nil && ptype == "commit" {
			oldTree = parseCommit(pbody).Tree
		}
	}

	var touched []string
	for _, p := range paths {
		oldSHA, err := treeAtSHA(r, oldTree, p)
		if err != nil {
			return nil, err
		}
		newSHA, err := treeAtSHA(r, newTree, p)
		if err != nil {
			return nil, err
		}
		if oldSHA != newSHA {
			touched = append(touched, p)
		}
	}
	return touched, nil
}

// treeAtSHA 在给定根树下解析路径，返回子树 SHA。
func treeAtSHA(r *Repo, rootTree, path string) (string, error) {
	path = strings.Trim(path, "/")
	if rootTree == "" {
		return "", nil
	}
	if path == "" {
		return rootTree, nil
	}
	cur := rootTree
	for _, seg := range strings.Split(path, "/") {
		items, err := r.treeItems(cur)
		if err != nil {
			return "", err
		}
		found := ""
		for _, it := range items {
			if it.name == seg && it.mode == "40000" {
				found = it.sha
				break
			}
		}
		if found == "" {
			return "", nil
		}
		cur = found
	}
	return cur, nil
}

// CommitInfoAt 解析一个 commit SHA 的元信息（树与父提交）。
//
// 供上层做「这次提交动了哪些子树」的差量计算：父提交就是差分基线。
func (r *Repo) CommitInfoAt(sha string) (tree string, parents []string, err error) {
	if err := validSHA(sha); err != nil {
		return "", nil, err
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return "", nil, err
	}
	if typ != "commit" {
		return "", nil, fmt.Errorf("对象 %s 不是 commit，而是 %s", sha, typ)
	}
	c := parseCommit(body)
	return c.Tree, c.Parents, nil
}

// IsAncestor reports whether ancestor is an ancestor (or equal) of descendant.
// It follows commit parent links only and never shells out to git.
func (r *Repo) IsAncestor(ancestor, descendant string) (bool, error) {
	if err := validSHA(ancestor); err != nil {
		return false, err
	}
	if err := validSHA(descendant); err != nil {
		return false, err
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if sha == ancestor {
			return true, nil
		}
		if seen[sha] {
			continue
		}
		seen[sha] = true
		_, parents, err := r.CommitInfoAt(sha)
		if err != nil {
			return false, err
		}
		queue = append(queue, parents...)
	}
	return false, nil
}
