//go:build perf

package gitstore

import (
	"fmt"
	"testing"
)

// 连续写入超过队列上限时，不能把失败推给调用方。
//
// 要防的具体故障：早先 dirtyQueueCapacity = 256，一轮会话生成几十条推理链、
// 或批量导入脚本连写几千条时，worker 只要慢几毫秒，第 257 条就被直接拒绝：
//
//	dirty write queue is full (256 pending); refusing new writes, retry later
//
// 而调用方（agent 插件、导入脚本、对端同步）几乎不可能自己实现「等一会儿
// 再试」——一次正常写入就此失败，且失败原因对它毫无意义。
//
// 现在的行为：满了先背压等待 worker 排水，通常几毫秒就能继续；只有磁盘
// 真卡死、等到 5 秒上限才报错。
func TestWriteSurvivesQueueFull(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// 写 1500 条：约 1.5 倍于队列容量，确保真的触发过背压。
	const N = 1500
	for i := 0; i < N; i++ {
		path := fmt.Sprintf("2026-11-19/%05d.md", i)
		if err := r.Write("zero", path, []byte("body "+fmt.Sprint(i)), ModeContent); err != nil {
			t.Fatalf("第 %d 条写入失败（队列满不应直接拒绝）：%v", i, err)
		}
	}
	if got := r.BackpressureCount(); got == 0 {
		t.Fatalf("写了 %d 条却没有触发过背压，说明容量测试没有真正覆盖满队列路径", N)
	} else {
		t.Logf("背压触发 %d 次，%d 条写入全部成功", got, N)
	}

	// 数据必须完整：不能因为背压丢了任何一条。
	entries, err := r.List("zero")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != N {
		t.Fatalf("应得 %d 条，实际 %d 条（背压路径丢数据）", N, len(entries))
	}
}

// 删除同样不能在队列满时直接失败。
func TestDeleteSurvivesQueueFull(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const N = 400
	for i := 0; i < N; i++ {
		if err := r.Write("zero", fmt.Sprintf("d/%05d.md", i), []byte("x"), ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < N; i++ {
		if err := r.Delete("zero", fmt.Sprintf("d/%05d.md", i)); err != nil {
			t.Fatalf("第 %d 条删除失败：%v", i, err)
		}
	}
	entries, err := r.List("zero")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("删除后应为空，实际还剩 %d 条", len(entries))
	}
}
