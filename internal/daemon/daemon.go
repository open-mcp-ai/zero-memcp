// Package daemon 管理 `zero-memcp daemon` 子命令背后的后台实例：
// 拉起它、停止它、报告它的状态。
//
// 平台相关的分离与终止机制抄自 termcp 的同名包——这块自己写最容易在
// Windows 上翻车（前台/后台、控制台事件、进程组），而它是调试成本最高、
// 复用价值也最高的一小段代码。
//
// 与 termcp 的差别：termcp 的 daemon 需要通过 HTTP 端点发现实例（因为它
// 的数据目录可以被多次指定）。zero-memcp 的实例身份由 pidfile +
// /health 共同确定，简单一层就够。
package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pidFileName 是数据目录下的实例标记文件名。
const pidFileName = "daemon.pid"

// PidPath 返回某数据目录下的 pidfile 路径。
func PidPath(dataDir string) string { return filepath.Join(dataDir, pidFileName) }

// WritePid 记录当前进程号。启动时调用，退出时由调用方清理。
func WritePid(dataDir string, pid int) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(PidPath(dataDir), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// ReadPid 读出 pidfile 里的进程号；不存在或内容非法时返回 0。
func ReadPid(dataDir string) int {
	b, err := os.ReadFile(PidPath(dataDir))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// ClearPid 删除 pidfile。停止后调用；文件不存在视为成功。
func ClearPid(dataDir string) error {
	err := os.Remove(PidPath(dataDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Status 是对外报告的一份实例状态。
type Status struct {
	Pid      int   `json:"pid"`
	Alive    bool  `json:"alive"`
	Healthy  bool  `json:"healthy"`
	HealthMS int64 `json:"health_ms"`
}

// String 让人读得懂。
func (s Status) String() string {
	if s.Pid == 0 {
		return "未运行（无 pidfile）"
	}
	if !s.Alive {
		return fmt.Sprintf("未运行（pidfile 残留 pid=%d，进程已不存在）", s.Pid)
	}
	if !s.Healthy {
		return fmt.Sprintf("进程在（pid=%d）但 %s 不响应", s.Pid, "/health")
	}
	return fmt.Sprintf("运行中（pid=%d，健康检查 %dms）", s.Pid, s.HealthMS)
}

// Inspect 采集一份状态。healthURL 形如 http://127.0.0.1:1729/health。
//
// 只看 pidfile 不够：进程可能崩了但文件还在，也可能 pid 被系统复用。
// 所以「活着」要由 ProcessAlive 和 /health 两侧共同确认。
func Inspect(dataDir, healthURL string) Status {
	st := Status{Pid: ReadPid(dataDir)}
	if st.Pid == 0 {
		return st
	}
	st.Alive = ProcessAlive(st.Pid)
	if !st.Alive {
		return st
	}
	if healthURL != "" {
		start := time.Now()
		client := &http.Client{Timeout: 1500 * time.Millisecond}
		res, err := client.Get(healthURL)
		st.HealthMS = time.Since(start).Milliseconds()
		if err == nil {
			res.Body.Close()
			st.Healthy = res.StatusCode == http.StatusOK
		}
	}
	return st
}

// Stop 停止 pidfile 记录的实例。
//
// 先试优雅终止（Unix 上是 SIGTERM，serve 已注册处理），失败再强制。
// Windows 上没有可送达的信号，terminate 直接硬杀——这对 zero-memcp 是
// 安全的：仓库本身是 Git 对象库，脏层未折叠的内容留在磁盘上，下次启动
// 会重新加载，不存在「半写」的中间状态。
func Stop(dataDir string) error {
	pid := ReadPid(dataDir)
	if pid == 0 {
		return fmt.Errorf("没有运行中的实例（%s 不存在或为空）", PidPath(dataDir))
	}
	if !ProcessAlive(pid) {
		_ = ClearPid(dataDir)
		return fmt.Errorf("pid=%d 的进程已不存在，已清理残留 pidfile", pid)
	}
	if err := terminate(pid); err != nil {
		return err
	}
	// 等它真的退出：SIGTERM 是请求，不是保证。
	for i := 0; i < 100; i++ {
		if !ProcessAlive(pid) {
			_ = ClearPid(dataDir)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("已经发送终止信号，但 pid=%d 在 5 秒内仍未退出", pid)
}
