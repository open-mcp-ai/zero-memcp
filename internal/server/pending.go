package server

// pending 是 `diff <branch>` 单参数形式的别名。
//
// 为什么不保留两份实现：两个动词同一件事，两份实现迟早会漂移。
// 留 pending 这个名字是因为它在归纳流程里读起来更直白
// （「还有哪些没提交」），而 diff 是 git 的直觉。
func (s *Server) execPending(rest string, c Caller, defaultBranch string) (string, error) {
	return s.execUncommittedDiff(rest, c, defaultBranch)
}
