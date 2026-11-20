package gitstore

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrBlobMissing 表示工作层要引用的 blob 在本仓库里不存在。
//
// 它是「先 tree 后 blob」这一顺序的守门人：调用方拿到它就该去对端
// 拉这个对象，而不是在这里造一个空文件顶上。
var ErrBlobMissing = errors.New("blob is not present in this repository")

// ApplyChange 把一条工作层变更直接落到脏层，**不提交**。
//
// 与 Write 的区别：Write 收的是正文，自己算 blob；这里收的是**已经
// 存在的 blob 哈希**。工作层同步正是这个形态——对端给的是一条
// 「某路径指向某 blob」的事实，内容本身按需再拉。
//
// 为什么必须先确认 blob 存在：脏层只记 (mode, sha)，正文在 objects/ 里。
// 若放行一个不存在的 sha，脏层会指向空气——读的时候才炸，而且爆在
// 完全无关的地方（比如某次 recall）。
func (r *Repo) ApplyChange(ref string, path string, mode Mode, sha string, deleted bool) error {
	normRef := normalizeRef(ref)
	if err := ValidateBranch(normRef); err != nil {
		return err
	}
	if err := validPath(path); err != nil {
		return err
	}
	if !deleted {
		if mode == "" {
			mode = ModeContent
		}
		if err := validSHA(sha); err != nil {
			return fmt.Errorf("apply %s: %w", path, err)
		}
		if !r.HasObject(sha) {
			return fmt.Errorf("apply %s: blob %s: %w", path, sha, ErrBlobMissing)
		}
	}

	dir, name := splitDirName(path)

	unlockDirty := r.rlockDirty()
	defer unlockDirty()

	unlock, lerr := r.lockDirtyFile(normRef, dir)
	if lerr != nil {
		return lerr
	}
	defer unlock()

	n, err := r.loadDirtyNode(normRef, dir)
	if err != nil {
		return err
	}
	if n == nil {
		baseTree := ""
		if c, cerr := r.commitOf(normRef); cerr == nil {
			baseTree = c.Tree
		}
		items, cerr := r.cleanItemsAt(baseTree, dir)
		if cerr != nil {
			return cerr
		}
		n = startDirtyNode(items)
	}
	if deleted {
		delete(n.set, name)
		n.del[name] = true
	} else {
		n.set[name] = dirtyEntry{Mode: mode, SHA: sha}
		delete(n.del, name)
	}
	return r.saveDirtyNode(normRef, dir, n)
}

// BlobBody 读出一个 blob 的正文。
//
// 只放行 blob 类型：工作层同步绝不该把 tree/commit 当文件内容搬走。
func (r *Repo) BlobBody(sha string) ([]byte, error) {
	typ, body, err := r.readObject(sha)
	if err != nil {
		return nil, err
	}
	if typ != "blob" {
		return nil, fmt.Errorf("object %s is a %s, not a blob", sha, typ)
	}
	return body, nil
}

// PutBlob 落一个对端送来的 blob，**先自算哈希再写**。
//
// 这是「发出去了不代表真的发了」的接收端一半：内容对自己算一遍，
// 与声明的名字不符就拒绝。用 writeObject 而不是直接写文件，是为了
// 复用它已有的「按内容寻址、已存在则跳过」语义。
func (r *Repo) PutBlob(sha string, body []byte) error {
	head := fmt.Sprintf("blob %d\x00", len(body))
	full := append([]byte(head), body...)
	sum := sha1.Sum(full)
	if got := hex.EncodeToString(sum[:]); got != sha {
		return fmt.Errorf("blob content hashes to %s, not %s", got, sha)
	}
	written, err := r.writeObject("blob", body)
	if err != nil {
		return err
	}
	if written != sha {
		return fmt.Errorf("writeObject produced %s but %s was expected", written, sha)
	}
	return nil
}
