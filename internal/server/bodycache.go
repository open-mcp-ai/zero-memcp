package server

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// 正文缓存（body cache）：按 blob 哈希缓存解压后的正文。
//
// # 为什么需要
//
// grep 无 scope 时要把全库正文读出来做正则。实测 2,000 块耗时 594ms，
// 第二次仍是 568ms——说明每次都在从头解压。按线性外推，38,000 篇约
// 11 秒，而 MCP 调用有超时，症状就是「grep 老是超时」。
//
// readBlock 本来就有一个 Block.Body 缓存，但它的定位是「扫描到索引
// 这段窗口里的临时缓存」，索引建完必须清空（否则整库正文常驻内存，
// 见 blockbody_cache_test.go）。于是稳态下 grep 每次都从 Git 对象里
// 重新 zlib 解压一遍全库。
//
// # 为什么用 blob 哈希做键
//
// 内容寻址自带失效语义：内容变一个字，哈希就变，旧条目自然不再被
// 命中。于是这个缓存**不需要任何失效逻辑**——没有 TTL、没有版本号、
// 没有「改完之后记得清一下」这种迟早会漏的约定。
//
// # 为什么可以不加锁
//
// 每个 key 独立，没有跨 key 不变量，正是 sync.Map 的适用面。
// 字节预算用 atomic 累加：宁可偶尔超出一点，也不为一个计数值引入锁。
// 超出预算就停止写入新条目（已有的继续服务），这样内存有上界，
// 而最热的那些（反复被 grep 到的）会稳定留在里面。
type bodyCache struct {
	// budget 是允许占用的总字节数；<=0 表示完全禁用。
	budget int64
	used   atomic.Int64
	m      sync.Map // blob hash -> []byte
	// stored / hits 只用于诊断，出现在 status 里。
	stored atomic.Int64
	hits   atomic.Int64
	// skipped 统计因超预算而没存的次数——它说明预算给小了。
	skipped atomic.Int64
}

func newBodyCache(budget int64) *bodyCache {
	return &bodyCache{budget: budget}
}

// get 取一份正文。第二个返回值表示是否命中。
func (c *bodyCache) get(hash string) ([]byte, bool) {
	if c == nil || c.budget <= 0 || hash == "" {
		return nil, false
	}
	if v, ok := c.m.Load(hash); ok {
		c.hits.Add(1)
		return v.([]byte), true
	}
	return nil, false
}

// put 存一份正文。超预算就不存（不驱逐已有的）。
//
// 不实现 LRU 驱逐是刻意的：LRU 需要维护访问顺序，而并发下维护顺序
// 就得上锁。这里的取舍是「内存有上界 + 热数据自然留下」，代价是
// 冷数据偶尔挤不进来——对 grep 这种全库扫描型负载，命中率已经够高。
func (c *bodyCache) put(hash string, body []byte) {
	if c == nil || c.budget <= 0 || hash == "" || len(body) == 0 {
		return
	}
	size := int64(len(body))
	if c.used.Load()+size > c.budget {
		c.skipped.Add(1)
		return
	}
	if _, loaded := c.m.LoadOrStore(hash, body); loaded {
		return
	}
	c.used.Add(size)
	c.stored.Add(1)
}

// stats 给 status 报告用。
func (c *bodyCache) stats() (stored, hits, skipped int64, usedMB float64) {
	if c == nil {
		return 0, 0, 0, 0
	}
	return c.stored.Load(), c.hits.Load(), c.skipped.Load(),
		float64(c.used.Load()) / (1 << 20)
}

// searchBodyBudget 从配置取正文缓存预算。
func searchBodyBudget(f *config.File) int64 {
	if f == nil {
		return config.SearchConfig{}.BodyCacheBudget()
	}
	return f.Search.BodyCacheBudget()
}

// statusLine 给 status 报告缓存状态。
//
// 为什么必须可见：缓存一旦被配成 0（或者预算给小了导致 skipped 暴涨），
// 症状只有「grep 变慢」——而这在网络、磁盘、CPU 都在变化的环境里
// 极难归因。把它摆在 status 里，运维一眼就能看出是缓存没生效。
func (c *bodyCache) statusLine() string {
	if c == nil {
		return "body cache: disabled"
	}
	stored, hits, skipped, usedMB := c.stats()
	if c.budget <= 0 {
		return "body cache: disabled (grep re-reads every block from git objects)"
	}
	line := fmt.Sprintf("body cache: %d block(s), %.1f/%.0f MB, %d hit(s)",
		stored, usedMB, float64(c.budget)/(1<<20), hits)
	if skipped > 0 {
		line += fmt.Sprintf(", %d skipped by budget", skipped)
	}
	return line
}

// putCount 返回累计插入条数（测试用）。
func (c *bodyCache) putCount() int64 {
	if c == nil {
		return 0
	}
	return c.stored.Load()
}

// hitCount 返回累计命中次数（测试用）。
func (c *bodyCache) hitCount() int64 {
	if c == nil {
		return 0
	}
	return c.hits.Load()
}
