package server

import "sync/atomic"

// backgroundRun 是一个后台循环的生命周期句柄。
//
// 只有循环真正的所有者持有它：start 成功的人拿到它，stop 成功的人关掉它。
type backgroundRun struct {
	stop chan struct{}
	kick chan struct{} // 容量 1：写入后请求尽快跑一次，满了就是已有待办
}

// backgroundRunner 表达「最多只有一个实例在跑」的后台循环。
//
// 为什么不用 Mutex 包一个 stop/kick 字段：
//
//	Lock(); if stop != nil { Unlock(); return }; stop = ...; Unlock()
//
// 这段的「检查并置位」不是原子的 —— 它靠人保证两条 return 路径都配平了锁。
// 而 CompareAndSwap(nil, run) 把「检查 + 置位」合成一次原子操作，
// 对错当场可判，不需要跨行推理。
//
// 零值可用（未启动状态）。
type backgroundRunner struct {
	cur atomic.Pointer[backgroundRun]
}

// start 尝试成为唯一的所有者。返回 (run, true) 表示本次启动成功，
// 调用方负责起 goroutine 并在循环里读 run.stop / run.kick。
// 返回 (nil, false) 表示已经有一个在跑。
func (r *backgroundRunner) start() (*backgroundRun, bool) {
	run := &backgroundRun{
		stop: make(chan struct{}),
		kick: make(chan struct{}, 1),
	}
	if !r.cur.CompareAndSwap(nil, run) {
		return nil, false
	}
	return run, true
}

// stopNow 停掉正在跑的循环（若有）。可重复调用。
func (r *backgroundRunner) stopNow() {
	if run := r.cur.Swap(nil); run != nil {
		close(run.stop)
	}
}

// kick 请求尽快跑一次。非阻塞、自动去重；没在跑时静默忽略。
func (r *backgroundRunner) kick() {
	run := r.cur.Load()
	if run == nil {
		return
	}
	select {
	case run.kick <- struct{}{}:
	default:
		// 已有一笔待执行，全量追平一次就够，丢掉这笔请求。
	}
}
