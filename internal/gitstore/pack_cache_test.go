package gitstore

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

// 外部 `git gc` 之后，本进程必须仍然读得到对象。
//
// README 把「gc」当成一个可选优化，并声明对象落在标准 Git 布局里
// （`git fsck` 校验通过、外部 clone 能读）。那么反过来也必须成立：
// 外部 gc 换掉 objects/pack 下的文件后，服务不能瞎掉。
//
// 曾经的失效方式：pack 索引缓存的键只有仓库路径，没有任何版本探测。
// 缓存一旦填过，gc 之后仍然用旧索引——于是所有对象都报
// 「不在松散库也不在任何 pack 里」，症状是「记忆凭空消失」，
// 而错误信息里没有任何线索指向缓存。
func TestExternalGCReloadsPackIndex(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有装 git，跳过")
	}
	dir := filepath.Join(t.TempDir(), "repo.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for i := 0; i < 3; i++ {
		p := fmt.Sprintf("a/%02d.md", i)
		if err := r.Write("zero", p, []byte("内容 "+p), ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}
	// 自己先打一次包，把缓存填上（这是触发条件：缓存必须非空）。
	if _, err := r.Repack(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read("zero", "a/00.md"); err != nil {
		t.Fatalf("打包后应当能读: %v", err)
	}

	// 再写一条新内容，然后让外部真 git 全量重打包。
	if err := r.Write("zero", "b/new.md", []byte("外部 gc 之后还要读得到"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "second"); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command("git", "-C", dir, "gc", "--aggressive", "--prune=now").CombinedOutput(); err != nil {
		t.Fatalf("外部 git gc 失败: %v\n%s", err, out)
	}

	if _, err := r.Read("zero", "b/new.md"); err != nil {
		t.Errorf("外部 gc 之后读新对象失败: %v", err)
	}
	if _, err := r.Read("zero", "a/00.md"); err != nil {
		t.Errorf("外部 gc 之后读旧对象失败: %v", err)
	}
	entries, err := r.List("zero")
	if err != nil {
		t.Errorf("外部 gc 之后列目录失败: %v", err)
	}
	if len(entries) != 4 {
		t.Errorf("外部 gc 之后应当看到 4 个文件，得到 %d 个", len(entries))
	}
}

// 缓存命中时不该每次重读索引文件：指纹一致就直接返回。
func TestPackCacheHitsWithoutChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Write("zero", "a.md", []byte("x"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Repack(); err != nil {
		t.Fatal(err)
	}

	first, err := r.loadPacks()
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.loadPacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("索引数变了: %d -> %d", len(first), len(second))
	}
	// 同一份缓存：切片里的指针应当相同（没有重读）。
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("第 %d 个索引被重读了（缓存未命中）", i)
		}
	}
}
