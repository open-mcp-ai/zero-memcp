package gitstore

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 分支锁：把一条分支锁住，只留 root 能碰。
//
// 为什么锁是 ref 而不是配置文件里的一行：
//   - 配置要 reload 才生效，而「现在立刻停手」不能等一次 reload；
//   - 配置是全局的，锁是某一个仓库某一条分支的局部事实；
//   - ref 写入是原子的，且天然记录「锁在哪个提交上」——
//     处理完对比一下就能知道冻结期间有没有人绕过。
//
// 锁状态存在仓库内的 zero-memcp/locks/，不使用 Git refs：
//   - 它是服务端本地权限元数据，不属于 Git 历史，也不应被 clone/push；
//   - git pack-refs / gc 不能把它移入 packed-refs，unlock 永远能删除；
//   - branch 名先 base64 编码成单一文件名，不会构造路径。
const lockDirName = "zero-memcp/locks"

// LockBranch 锁住一条分支，记录当时指向的提交。
//
// 记录提交不是为了回滚，是为了对照：解锁时能看出这段时间里
// 「有没有人（比如 root 自己）动过它」。
func (r *Repo) LockBranch(branch string) (string, error) {
	name := normalizeRef(branch)
	sha, err := r.Resolve(name)
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "", fmt.Errorf("分支 %q 不存在或没有提交", branch)
	}
	path := r.lockPath(name)
	if err := r.FS().MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := r.FS().WriteAtomic(path, []byte(sha+"\n"), 0o600); err != nil {
		return "", err
	}
	return sha, nil
}

// UnlockBranch 解除锁定。未加锁时返回 false。
func (r *Repo) UnlockBranch(branch string) (bool, error) {
	name := normalizeRef(branch)
	path := r.lockPath(name)
	if _, err := r.FS().Stat(path); err != nil {
		if isNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := r.FS().Remove(path); err != nil && !isNotExist(err) {
		return false, err
	}
	return true, nil
}

// BranchLocked 报告分支是否被锁，以及加锁时它指向的提交。
func (r *Repo) BranchLocked(branch string) (bool, string) {
	b, err := r.FS().ReadFile(r.lockPath(normalizeRef(branch)))
	if err != nil {
		return false, ""
	}
	sha := strings.TrimSpace(string(b))
	if validSHA(sha) != nil {
		return false, ""
	}
	return true, sha
}

// LockedBranches 列出被锁的分支名。
func (r *Repo) LockedBranches() []string {
	root := filepath.Join(r.Path, lockDirName)
	var out []string
	_ = r.FS().WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name, decErr := base64.RawURLEncoding.DecodeString(d.Name())
		if decErr == nil {
			out = append(out, string(name))
		}
		return nil
	})
	return out
}

// lockPath 返回一条分支的本地锁标记文件。
func (r *Repo) lockPath(branch string) string {
	name := base64.RawURLEncoding.EncodeToString([]byte(branch))
	return filepath.Join(r.Path, lockDirName, name)
}
