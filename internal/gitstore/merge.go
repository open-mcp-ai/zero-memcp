package gitstore

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// MergeResult 描述一次合并的结局。
type MergeResult struct {
	// Kind 是结局类型：up-to-date / ahead / fast-forward / merged / conflict
	Kind string
	// Commit 是合并后分支指向的 commit（conflict 时为空）。
	Commit string
	// Conflicts 是内容冲突的路径（仅 conflict 时非空）。
	Conflicts []string
	// Added 是本次合并新增的路径数（仅 merged / fast-forward 有意义）。
	Added int
}

// MergeInto 把 other 分支合并进 branch。
//
// 这是「对等同步」的落地原语。它只做**树层面**的合并，不做内容层面的三方合并——
// 因为记忆库里同一个路径出现不同内容，意味着两台机器对同一件事写下了不同的话，
// 那不是能自动解决的冲突，是要人判断的。宁可不合并，也不能悄悄挑一边。
//
// 合并规则：
//   - 两边同路径同 SHA          → 保留
//   - 一边有、另一边没有        → 采用有的一边（并集）
//   - 两边同路径不同 SHA        → 冲突，整次合并拒绝
//
// 冲突时不留半成品：新 tree 和 commit 都不写，分支指针不动。
// 调用方拿到 Conflicts 后可以决定人工怎么处理。
func (r *Repo) MergeInto(branch, other string) (MergeResult, error) {
	normRef := normalizeRef(branch)
	if err := ValidateBranch(normRef); err != nil {
		return MergeResult{}, err
	}
	if err := r.syncDirtyWriter(); err != nil {
		return MergeResult{}, err
	}

	release, err := r.acquireCommitToken(normRef)
	if err != nil {
		return MergeResult{}, err
	}
	defer release()

	unlockDirty := r.lockDirty()
	defer unlockDirty()

	// 脏层里还有未提交的东西时拒绝合并。
	//
	// 理由：合并会推进分支指针，而脏层是「相对旧指针的改动」。
	// 先提交再合并，脏层的语义才清晰；否则合并后的树里，
	// 脏层的改动会以「未提交」的形态漂浮在一个新基线上。
	dirs, err := r.listDirtyDirs(normRef)
	if err != nil {
		return MergeResult{}, err
	}
	if len(dirs) > 0 {
		return MergeResult{}, fmt.Errorf(
			"branch %q has %d uncommitted change(s); commit or discard them before merging", normRef, len(dirs))
	}

	localSHA, err := r.Resolve(normRef)
	if err != nil {
		return MergeResult{}, err
	}
	otherSHA, err := r.Resolve(other)
	if err != nil {
		return MergeResult{}, fmt.Errorf("resolve %s: %w", other, err)
	}
	if localSHA == otherSHA {
		return MergeResult{Kind: "up-to-date", Commit: localSHA}, nil
	}

	// 快进判定：本地是对方祖先 → 直接推进；对方是本地祖先 → 无事可做。
	if localSHA != "" {
		otherHasLocal, err := r.IsAncestor(localSHA, otherSHA)
		if err == nil && otherHasLocal {
			// other 包含 local：本地落后，可以快进
			if err := r.updateRef("refs/heads/"+normRef, otherSHA); err != nil {
				return MergeResult{}, fmt.Errorf("fast-forward ref: %w", err)
			}
			return MergeResult{Kind: "fast-forward", Commit: otherSHA}, nil
		}
		localHasOther, err := r.IsAncestor(otherSHA, localSHA)
		if err == nil && localHasOther {
			return MergeResult{Kind: "ahead", Commit: localSHA}, nil
		}
	}

	// 分叉：展开两边树，逐路径合并
	localTree := map[string]Entry{}
	otherTree := map[string]Entry{}
	if localSHA != "" {
		c, err := r.commitOf(normRef)
		if err != nil {
			return MergeResult{}, err
		}
		if err := r.loadTree(c.Tree, "", localTree); err != nil {
			return MergeResult{}, err
		}
	}
	if otherSHA != "" {
		typ, body, err := r.readObject(otherSHA)
		if err != nil {
			return MergeResult{}, err
		}
		if typ != "commit" {
			return MergeResult{}, fmt.Errorf("%s is not a commit", other)
		}
		oc := parseCommit(body)
		if err := r.loadTree(oc.Tree, "", otherTree); err != nil {
			return MergeResult{}, err
		}
	}

	merged := map[string]Entry{}
	var conflicts []string
	for path, e := range localTree {
		merged[path] = e
	}
	for path, oe := range otherTree {
		le, exists := merged[path]
		if !exists {
			merged[path] = oe
			continue
		}
		if le.Hash != oe.Hash {
			conflicts = append(conflicts, path)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return MergeResult{Kind: "conflict", Conflicts: conflicts}, nil
	}

	// 无冲突：把 merged 写成新树
	rootTree, err := r.writeTreeFromPaths(merged)
	if err != nil {
		return MergeResult{}, err
	}

	// 两边的树本来可能一模一样（分叉但内容相同）——不写空提交
	if localSHA != "" {
		lc, err := r.commitOf(normRef)
		if err == nil && lc.Tree == rootTree {
			return MergeResult{Kind: "up-to-date", Commit: localSHA}, nil
		}
	}

	parents := []string{}
	if localSHA != "" {
		parents = append(parents, localSHA)
	}
	if otherSHA != "" {
		parents = append(parents, otherSHA)
	}
	// 父顺序确定化。
	//
	// [#] 为什么必须排序
	//
	// 不排序时父顺序是「我在哪一侧」——local 在前、other 在后。于是两个
	// peer 对**同一对提交**做一次 merge，除了父顺序之外树、消息、时间全相同，
	// 却得到两个不同的 SHA：
	//
	//	c6668915  parent 380ba18, 3b506ee   ← A
	//	5861d7d   parent 3b506ee, 380ba18   ← B
	//
	// 后果是两边永远推不上对方：非快进被拒。而因为树相同，
	// 「树一样就不写空提交」的检查会在两边都判 up-to-date，各自抱住
	// 自己的提交——分叉就永久停在那个状态，每轮同步重试一次、失败一次。
	//
	// 排序后两台机器产出逐字节相同的 commit，SHA 天然收敛。
	sort.Strings(parents)

	// 时间戳同样必须确定化：time.Now() 让两个 peer 只要差一秒就永远不合。
	// 取两亲提交时间的较大者——它只取决于输入，与「谁先算」无关，
	// 且不会让 merge 提交比它的父提交更旧。
	when := r.mergeTimestamp(parents)

	// 消息里不能出现 ref 名（`refs/remotes/<remote>/...`）：两个 peer 的
	// 远端命名可以不同，那会让 SHA 又分叉。用排好序的父提交短 SHA。
	shorts := make([]string, len(parents))
	for i, p := range parents {
		shorts[i] = p
		if len(shorts[i]) > 7 {
			shorts[i] = shorts[i][:7]
		}
	}
	msg := fmt.Sprintf("merge %s into %s", strings.Join(shorts, " "), normRef)
	commitSHA, err := r.writeCommit(rootTree, parents, msg, when)
	if err != nil {
		return MergeResult{}, err
	}
	if err := r.updateRef("refs/heads/"+normRef, commitSHA); err != nil {
		return MergeResult{}, fmt.Errorf("update ref: %w", err)
	}
	return MergeResult{Kind: "merged", Commit: commitSHA, Added: len(merged)}, nil
}

// writeTreeFromPaths 把「路径 → Entry」的扁平 map 重建成完整的树对象图。
//
// 做法：按目录分组，自底向上逐层写 tree。深度降序保证子目录先于父目录
// 被写入，父目录写的时候能拿到子目录的新 SHA。
func (r *Repo) writeTreeFromPaths(paths map[string]Entry) (string, error) {
	// 按目录分桶：dir -> []treeItem
	byDir := map[string][]treeItem{}
	for path, e := range paths {
		dir, name := splitDirName(path)
		byDir[dir] = append(byDir[dir], treeItem{mode: string(e.Mode), name: name, sha: e.Hash})
	}

	// 收集所有目录（含因文件嵌套而隐含的中间目录）
	allDirs := map[string]bool{"": true}
	for path := range paths {
		dir, _ := splitDirName(path)
		for dir != "" {
			allDirs[dir] = true
			parent, _ := splitDirName(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	dirs := make([]string, 0, len(allDirs))
	for d := range allDirs {
		dirs = append(dirs, d)
	}
	// 深度降序：先写最深目录
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if di != dj {
			return di > dj
		}
		return dirs[i] > dirs[j]
	})

	subtreeSHA := map[string]string{}
	for _, dir := range dirs {
		items := byDir[dir]
		// 补上子目录条目：本层不直接包含它们（它们是 dir/child 这样的键）
		prefix := ""
		if dir != "" {
			prefix = dir + "/"
		}
		for sub, sha := range subtreeSHA {
			if !strings.HasPrefix(sub, prefix) || sub == dir {
				continue
			}
			rest := strings.TrimPrefix(sub, prefix)
			if strings.Contains(rest, "/") {
				continue
			}
			items = append(items, treeItem{mode: "40000", name: rest, sha: sha})
		}
		sha, err := r.writeTreeItems(items)
		if err != nil {
			return "", err
		}
		subtreeSHA[dir] = sha
	}
	return subtreeSHA[""], nil
}

// mergeTimestamp 给 merge 提交取一个只由输入决定的时间。
//
// 取所有父提交时间的最大值，理由有三：
//
//  1. 确定性 —— 它只看提交对象，不看墙钟。两个 peer 算出同一个值。
//  2. 单调 —— merge 提交不能比它的父提交更旧，否则 git log 的拓扑顺序
//     与时间顺序会互相矛盾（rebase/fetch 的剪枝策略会因此判断错误）。
//  3. 无需信任时钟 —— 不依赖两台机器的钟是否同步。
//
// 读不到任何父提交时间时退回 time.Now()：这是异常路径（对象损坏），
// 此时确定性已经无从谈起，让合并先成功比卡在这里重要。
func (r *Repo) mergeTimestamp(parents []string) time.Time {
	var newest time.Time
	for _, p := range parents {
		typ, body, err := r.readObject(p)
		if err != nil || typ != "commit" {
			continue
		}
		if c := parseCommit(body); c.When.After(newest) {
			newest = c.When
		}
	}
	if newest.IsZero() {
		return time.Now()
	}
	return newest
}
