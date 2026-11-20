//go:build perf

package index

import (
	"fmt"
	"testing"
	"time"
)

// 倒排写入必须是 O(N)，不能退化成 O(N²)。
//
// 要防的具体故障：postingList.entries 是不可变切片，每次 add 都整份
// 复制（那是为了让读侧免锁）。单条 add 在活系统里没问题——一次只加
// 一篇。但冷建是「N 篇文档共享同一批常见词」，逐条 add 就退化成
// O(N²)：一个出现在全部 5,000 篇里的中文二元组，累计复制
// 0+1+…+4,999 ≈ 1,250 万条记录。
//
// 实测代价：5,000 篇冷建，分词 20ms、倒排写入 7.04 秒（348 倍）。
// 也就是说「分词慢」是个错觉，全耗在重复拷贝上。
//
// 这里用**规模翻倍的耗时比值**判定，而不是绝对时间：绝对时间随机器
// 波动，而 O(N²) 的比值是 4x、O(N) 的比值是 2x，差距大到不会误判。
func TestAddBatchPostingsLinearithmic(t *testing.T) {
	run := func(n int) time.Duration {
		docs := make([]BatchDoc, n)
		for i := 0; i < n; i++ {
			// 高度重叠的词表：每个文档都含同样的常见词，只有编号不同。
			// 这正是 O(N²) 会爆炸的形状。
			docs[i] = BatchDoc{
				Addr:    fmt.Sprintf("fact/%05d.md", i),
				Text:    fmt.Sprintf("共享的中文段落 english keywords 1729，独有编号 %d", i),
				ModTime: int64(i),
			}
		}
		ix := New()
		start := time.Now()
		ix.AddBatch(docs, 8)
		return time.Since(start)
	}

	small := run(1000)
	large := run(2000)

	if small < time.Millisecond {
		small = time.Millisecond // 防除零
	}
	ratio := float64(large) / float64(small)
	t.Logf("1000 篇 %v / 2000 篇 %v（比值 %.2fx，O(N) 应接近 2x，O(N²) 会到 4x）",
		small, large, ratio)

	// 留足余量：2x 是理论值，噪声与 GC 会推高。但 O(N²) 会是 4x 以上，
	// 3x 这个界能把两者分开。
	if ratio > 3.0 {
		t.Fatalf("规模翻倍耗时涨了 %.2fx，倒排写入退化成 O(N²) 了："+
			"检查是否又在逐条 add 而不是按词聚合（addMany）", ratio)
	}
}
