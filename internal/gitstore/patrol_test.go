package gitstore

import (
	"os"
	"path/filepath"
	"testing"
)

// 回读巡检必须能发现「文件在但内容坏」——这是 Stat 快路径的盲区。
//
// 要防的具体案子：断电/磁盘满之后留下一个零长度或截断的对象文件。
// 它 Stat 成功，于是每次写入都跳过；此后任何一次读都会拿到坏数据，
// 表现为「某篇记忆莫名不见了」。
func TestPatrolDetectsCorruptObject(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(filepath.Join(dir, "repo.git"))
	if err != nil {
		t.Fatal(err)
	}
	// 直接造松散对象（Write 走异步队列，此处要的是确定的落盘形态）。
	sha, err := r.writeObject("blob", []byte("hello patrol"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.objectPath(sha)); err != nil {
		t.Fatalf("对象应已落盘: %v", err)
	}

	// 把它的内容截断成垃圾。
	if err := os.Chmod(r.objectPath(sha), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.objectPath(sha), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	checked, corrupt, bad, err := r.PatrolObjects(100)
	if err != nil {
		t.Fatalf("巡检报错: %v", err)
	}
	if checked == 0 {
		t.Fatal("巡检应至少检查到一个对象")
	}
	if corrupt == 0 {
		t.Fatal("被截断的对象应当被判为损坏，实际巡检说全好")
	}
	if len(bad) == 0 {
		t.Fatal("损坏对象应留证地址")
	}
	t.Logf("巡检 checked=%d corrupt=%d 样本=%v", checked, corrupt, bad)
}

// 抽检命中损坏文件时，writeObject 必须覆盖重写而不是继续信任它。
func TestWriteObjectRepairsCorruptFileOnSampledPath(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(filepath.Join(dir, "repo.git"))
	if err != nil {
		t.Fatal(err)
	}
	// 每次都抽检，让测试确定而非概率。
	r.patrol.WriteSampleEvery = 1
	writeCounter.Store(0)

	sha, err := r.writeObject("blob", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	// 破坏它
	objPath := r.objectPath(sha)
	if err := os.Chmod(objPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyObject(sha); err == nil {
		t.Fatal("刚破坏的对象应当验不过")
	}

	// 再次写入同样内容：抽检命中损坏 → 覆盖重写 → 之后能验过
	sha2, err := r.writeObject("blob", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if sha2 != sha {
		t.Fatalf("内容相同应得同一哈希: %s vs %s", sha, sha2)
	}
	if err := r.VerifyObject(sha); err != nil {
		t.Fatalf("重写后对象应当完好，实际: %v", err)
	}
}
