package server

import (
	"fmt"
	"time"
)

// writeTimeout 是「写令牌」等待的上界。
//
// 为什么所有写令牌都要有超时：用 sync.Mutex 的人得到的语义是「等到别人放」，
// 而这个等待**没有上界**。一旦持锁者卡在磁盘 I/O、网络 I/O、或第三方回调里，
// 后面每一个调用者都会把自己的 goroutine 永久停在 Lock() 上，且持锁者还在动。
// termcp/AGENTS.md:35 记的就是这个形状：一个卡住的持锁者把后续所有请求排成一队，
// 最终整个实例挂起（#189、#206、#207、#212、#218–#222）。
//
// 给等待设一个明确的上界，最坏结果就从「整个进程挂死」退化为「这一次请求报错」。
const writeTimeout = 5 * time.Second

// tokenAcquire 取一个一槽 channel 令牌。
//
// 用法：callers 各自持有一个 `chan struct{}`（cap 1），写之前拿、写之后还。
// 语义上它是「串行化」，但和 Mutex 有三处关键差别：
//
//  1. 等待有上界 —— 超时返回错误，而不是无限期阻塞；
//  2. 读者完全不参与 —— 典型用法是「令牌只串行化写者，读者读原子快照」；
//  3. 归还不会因为 return/panic 漏掉 —— 调用方拿到的是显式的 done 闭包，
//     配合 defer 写在同一个作用域里，而 Lock/Unlock 要跨越多条 return 路径。
func tokenAcquire(ch chan struct{}, what string) (func(), error) {
	// 快路径：没人持有，直接拿，不分配 timer。
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	default:
	}
	t := time.NewTimer(writeTimeout)
	defer t.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-t.C:
		return nil, fmt.Errorf("%s: 写令牌等待超时（%v），有写者卡在 I/O 上", what, writeTimeout)
	}
}
