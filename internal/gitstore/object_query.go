package gitstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HasObject 报告对象是否存在于本地（松散或 pack 内）。
func (r *Repo) HasObject(sha string) bool {
	if err := validSHA(sha); err != nil {
		return false
	}
	if _, err := os.Stat(r.objectPath(sha)); err == nil {
		return true
	}
	packs, err := r.loadPacks()
	if err != nil {
		return false
	}
	var raw [20]byte
	for i := 0; i < 20; i++ {
		var b byte
		_, _ = fmt.Sscanf(sha[i*2:i*2+2], "%02x", &b)
		raw[i] = b
	}
	for _, p := range packs {
		if _, ok := p.find(raw); ok {
			return true
		}
	}
	return false
}

// SetRemoteRef 写入 refs/remotes/<remote>/<branch>。
//
// 只写远端跟踪引用，不动本地分支：fetch 的职责是「知道远端有什么」。
// ref 是本地状态的指针，可以有旧值，因此用 tmp + rename 原子替换。
func (r *Repo) SetRemoteRef(remote, branch, sha string) error {
	if err := validSHA(sha); err != nil {
		return err
	}
	if remote == "" || branch == "" {
		return fmt.Errorf("remote/branch 不能为空")
	}
	if strings.ContainsAny(remote+branch, " \t\r\n") || strings.Contains(branch, "..") {
		return fmt.Errorf("非法的 remote/branch：%q %q", remote, branch)
	}
	ref := "refs/remotes/" + remote + "/" + branch
	path := r.refPath(ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ref-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(sha + "\n"); err != nil {
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
	return os.Rename(tmpName, path)
}

// RemoteRef 读出 refs/remotes/<remote>/<branch> 的 sha，没有则返回空串。
func (r *Repo) RemoteRef(remote, branch string) string {
	sha, err := r.Resolve("refs/remotes/" + remote + "/" + branch)
	if err != nil {
		return ""
	}
	return sha
}

// DeleteRemoteRef 删除一个远端跟踪引用（远端已不存在该分支时）。
func (r *Repo) DeleteRemoteRef(remote, branch string) error {
	err := os.Remove(r.refPath("refs/remotes/" + remote + "/" + branch))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SetRef 写入任意 ref（本仓库自己的分支）。
//
// 与 SetRemoteRef 的区别只是名字：远端跟踪引用和本地分支的语义不同，
// 但落盘方式必须一样——tmp + rename，避免读到写了一半的指针。
func (r *Repo) SetRef(ref, sha string) error {
	if err := validSHA(sha); err != nil {
		return err
	}
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "/")
	if ref == "" || strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\r\n") {
		return fmt.Errorf("非法的 ref：%q", ref)
	}
	path := r.refPath(ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ref-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(sha + "\n"); err != nil {
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
	return os.Rename(tmpName, path)
}
