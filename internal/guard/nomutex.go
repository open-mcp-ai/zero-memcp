// Package guard 是编译期门禁：禁止在源码里出现 sync.Mutex / sync.RWMutex。
//
// # 为什么这是一道门禁而不是一条文档约定
//
// termcp 的 AGENTS.md 记着一份账：40 多个锁分 n 轮全部拆光，
// 拆的过程中清点出 10+ 次死锁（#189、#206、#207、#212、#218–#222）。
// 共同形状是同一个：**锁内做了网络/磁盘等待**，持锁者卡住，
// 后来者无限期挂着，进程表面活着但什么也不干了。
//
// zero-memcp 2026-11-16 那次 fatal error 是同一类问题的另一个出口：
// 索引是裸 map，靠调用方的 Server.mu 一条纪律保护。
// 纪律漏了两次，第二次是 runtime 直接杀进程——不是 error，没有 recover 机会。
//
// 所以这一条不做成「约定」：约定会被忘，门禁不会。
package guard

// NoMutex 让本包被 go build 编译到。
//
// 真正的检查在 nomutex_test.go：它扫描整个模块的源码 AST，
// 一旦发现 `sync.Mutex` / `sync.RWMutex` 的字段声明就失败。
//
// 为什么用测试而不是 build tag / go:generate：
// 生成器要有人记得跑；测试在 CI 和本地 `go test ./...` 里都会跑。
type NoMutex struct{}

// Allowed 记录「sync 包里哪些是可以用的」，供人查。
//
// 这不是豁免名单，是「这些原语不在禁用范围内」的说明：
// sync.Map 是并发安全的容器，不是锁；sync.Once 是一次性初始化，
// 没有「持锁等待」的语义；sync.WaitGroup 只做计数等待。
var Allowed = []string{
	"sync.Map",
	"sync.Once",
	"sync.WaitGroup",
}
