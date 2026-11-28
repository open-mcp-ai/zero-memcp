package gitstore

import (
	"os"
	"testing"
)

// 坏对象必须能被覆盖修好，而对象文件是 0444 只读。
//
// POSIX 的 rename 只看目录权限，能直接盖过只读文件，所以这个 bug 在
// macOS/Linux 上测不出来；Windows 的 MoveFileEx 遇到只读目标直接报
// Access is denied。实测 CI（windows）报：
//
//	persist object: rename …\objects\f8\.tmp-… …\objects\f8\e0804eaa…: Access is denied.
//
// 3060 就是 Windows，所以这条不是理论问题：抽检判定某个对象坏了之后，
// 「重写修好」那条路一直失败，坏对象会一直被信任下去。
func TestCorruptObjectCanBeRewrittenOnReadOnlyStore(t *testing.T) {
	r := newTestRepo(t)
	body := []byte("会被弄坏的内容")
	sha, err := r.BlobHash(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PutBlob(sha, body); err != nil {
		t.Fatal(err)
	}
	dst := r.objectPath(sha)
	if err := os.Chmod(dst, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("corrupted garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0o444); err != nil {
		t.Fatal(err)
	}

	// 强制抽检命中，让「不信任旧文件 → 覆盖重写」这条路真的走一遍。
	r.patrol.WriteSampleEvery = 1
	if err := r.PutBlob(sha, body); err != nil {
		t.Fatalf("坏对象必须能被修好（覆盖前要放开只读），实际: %v", err)
	}
	// 修好之后内容必须真的对了。
	if err := r.verifyObjectFile(dst, sha); err != nil {
		t.Fatalf("重写后内容仍然不对: %v", err)
	}
}

// 同一对象重复落盘（抽检命中）不许报错——那是幂等重投递。
func TestRepeatedBlobPutIsIdempotentUnderSampling(t *testing.T) {
	r := newTestRepo(t)
	body := []byte("重复投递的正文")
	sha, err := r.BlobHash(body)
	if err != nil {
		t.Fatal(err)
	}
	r.patrol.WriteSampleEvery = 1
	for i := 0; i < 3; i++ {
		if err := r.PutBlob(sha, body); err != nil {
			t.Fatalf("第 %d 次落盘失败: %v", i+1, err)
		}
	}
}
