package server

import (
	"fmt"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"strings"
	"sync/atomic"
	"time"
)

// 后台对象巡检（object patrol）。
//
// 为什么需要它：写入路径上那条 `Stat 成功 == 内容完好` 的快路径，在
// 断电、磁盘满、文件系统掉块之后会把人骗到底——坏对象存在，Stat 说在，
// 每次读取都以为拿到了好数据。等到有人发现某篇记忆搜不到，现场早已
// 被后续写入覆盖，无法定位。
//
// 这里做的是**低速存量扫描**：每轮看一百个对象，解压、重算 SHA-1、
// 对不上就记下来并在 status 里报。它不自动删除任何东西——坏对象是
// 证据，先让人看见，再由人决定是重建还是从对端回填。
type patrolState struct {
	checked     atomic.Int64
	corrupt     atomic.Int64
	lastBad     atomic.Pointer[string]
	lastRunNano atomic.Int64
	running     atomic.Bool
}

// startPatrol 启动后台巡检协程。
//
// interval <= 0 表示关闭（测试与「不在乎这个」的部署都用得上）。
func (s *Server) startPatrol(interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				s.patrolOnce(100)
			}
		}
	}()
}

// patrolOnce 扫一批对象。
func (s *Server) patrolOnce(batch int) {
	// 单飞：上一轮没跑完就跳过，避免慢磁盘上堆积。
	if !s.patrol.running.CompareAndSwap(false, true) {
		return
	}
	defer s.patrol.running.Store(false)
	s.patrol.lastRunNano.Store(time.Now().UnixNano())

	for _, repo := range s.allRepos() {
		checked, corrupt, bad, err := repo.PatrolObjects(batch)
		if err != nil {
			continue
		}
		s.patrol.checked.Add(int64(checked))
		if corrupt > 0 {
			s.patrol.corrupt.Add(int64(corrupt))
			sample := strings.Join(bad, ", ")
			s.patrol.lastBad.Store(&sample)
			// 必须出声：坏对象不会自愈，沉默等于放任。
			s.logf("patrol: WARNING %d corrupt object(s) in %s: %s", corrupt, repo.Path, sample)
		}
	}
}

// allRepos 返回当前打开的全部仓库。
func (s *Server) allRepos() []*gitstore.Repo {
	out := make([]*gitstore.Repo, 0, 8)
	for _, name := range s.projects.Names() {
		if r, err := s.projects.Get(name); err == nil && r != nil {
			out = append(out, r)
		}
	}
	return out
}

// patrolLines 给 status 报告巡检情况。
//
// 只报「查了多少、坏了多少」：这两个数能让运维判断「这套库还健康吗」，
// 而逐条列出几万个对象地址毫无信息量。
func (s *Server) patrolLines() []string {
	checked := s.patrol.checked.Load()
	if checked == 0 {
		return nil
	}
	line := fmt.Sprintf("objects: %d checked, %d corrupt", checked, s.patrol.corrupt.Load())
	if bad := s.patrol.lastBad.Load(); bad != nil && *bad != "" {
		line += " (sample: " + *bad + ")"
	}
	if t := s.patrol.lastRunNano.Load(); t > 0 {
		line += ", last run " + time.Unix(0, t).Format("15:04:05")
	}
	return []string{line}
}
