package server

import (
	"fmt"
	"sync"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// FanoutBatch 记录一次因收到推送而触发的扇出广播。
//
// 为什么需要独立批次记录：
// 收到外部推送后，向其他 peer 广播是一次有明确因果链的任务。
// 仅看全局的 lastSync 只能说明「某时刻跑过同步」，无法确认特定的
// 提交是否已同步至其他目标。
//
// 状态在内存中维护（保留最近 5 批），供 status 动词直接查看。
// 若进程重启，未完成的目标将交由启动时的常规同步流程兜底。
type FanoutBatch struct {
	ID        string                  `json:"id"`
	Project   string                  `json:"project"`
	Ref       string                  `json:"ref"`
	NewSHA    string                  `json:"new_sha"`
	Trigger   string                  `json:"trigger"`
	StartedAt time.Time               `json:"started_at"`
	DoneAt    time.Time               `json:"done_at,omitempty"`
	Completed bool                    `json:"completed"`
	Targets   map[string]FanoutTarget `json:"targets"`
}

type FanoutTarget struct {
	Status string    `json:"status"` // pending | running | done | failed | skipped
	At     time.Time `json:"at"`
	Err    string    `json:"err,omitempty"`
}

type fanoutHub struct {
	mu      sync.Mutex
	batches []*FanoutBatch
}

func newFanoutHub() *fanoutHub {
	return &fanoutHub{}
}

func (h *fanoutHub) add(b *FanoutBatch) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.batches = append([]*FanoutBatch{b}, h.batches...)
	if len(h.batches) > 5 {
		h.batches = h.batches[:5]
	}
}

func (h *fanoutHub) updateTarget(batchID, remoteName, status, errStr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.batches {
		if b.ID != batchID {
			continue
		}
		if b.Targets == nil {
			b.Targets = map[string]FanoutTarget{}
		}
		b.Targets[remoteName] = FanoutTarget{
			Status: status,
			At:     time.Now(),
			Err:    errStr,
		}
		// 检查是否全部目标都已处于终态
		allDone := true
		for _, t := range b.Targets {
			if t.Status == "pending" || t.Status == "running" {
				allDone = false
				break
			}
		}
		if allDone {
			b.Completed = true
			b.DoneAt = time.Now()
		}
		return
	}
}

// activeOrRecent 返回正在进行中、或完成不到 1 分钟的批次。
func (h *fanoutHub) activeOrRecent() []*FanoutBatch {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*FanoutBatch
	now := time.Now()
	for _, b := range h.batches {
		if !b.Completed || now.Sub(b.DoneAt) < 1*time.Minute {
			out = append(out, b)
		}
	}
	return out
}

// describeLines 为 status 动词格式化扇出状态。
func (h *fanoutHub) describeLines() []string {
	recent := h.activeOrRecent()
	if len(recent) == 0 {
		return nil
	}
	var out []string
	for _, b := range recent {
		age := time.Since(b.StartedAt).Truncate(time.Second)
		status := "broadcasting"
		if b.Completed {
			status = "completed"
		}
		short := b.NewSHA
		if len(short) > 7 {
			short = short[:7]
		}
		out = append(out, fmt.Sprintf("fanout: %s@%s -> %s (%s, %s, %s ago)",
			b.Project, b.Ref, short, b.Trigger, status, age))
		for name, t := range b.Targets {
			tAge := time.Since(t.At).Truncate(time.Second)
			line := fmt.Sprintf("  %s: %s (%s ago)", name, t.Status, tAge)
			if t.Err != "" {
				line += " [" + t.Err + "]"
			}
			out = append(out, line)
		}
	}
	return out
}

// onReceiveTargets 过滤出该项目下配置了 on_receive = true 的远端。
func (s *Server) onReceiveTargets(project string) []config.Remote {
	var out []config.Remote
	for _, t := range s.syncTargets() {
		if t.Project == project && t.Remote.OnReceive {
			out = append(out, t.Remote)
		}
	}
	return out
}

// triggerFanout 启动协程执行扇出，逐个记录目标状态。
func (s *Server) triggerFanout(project, ref, newSHA, trigger string) {
	targets := s.onReceiveTargets(project)
	if len(targets) == 0 {
		return
	}

	batch := &FanoutBatch{
		ID:        fmt.Sprintf("fanout-%s-%d", project, time.Now().UnixNano()),
		Project:   project,
		Ref:       ref,
		NewSHA:    newSHA,
		Trigger:   trigger,
		StartedAt: time.Now(),
		Targets:   make(map[string]FanoutTarget, len(targets)),
	}
	for _, rm := range targets {
		batch.Targets[rm.Name] = FanoutTarget{Status: "pending", At: time.Now()}
	}
	s.fanouts.add(batch)

	go func() {
		for _, rm := range targets {
			s.fanouts.updateTarget(batch.ID, rm.Name, "running", "")
			// 复用已有的 syncOnce 逻辑
			s.syncOnce(project, rm)
			// 根据 lastErr 确认是否出现错误
			key := project + "@" + rm.Name
			s.syncState.mu.Lock()
			errStr := s.syncState.lastErr[key]
			s.syncState.mu.Unlock()

			if errStr != "" {
				s.fanouts.updateTarget(batch.ID, rm.Name, "failed", errStr)
			} else {
				s.fanouts.updateTarget(batch.ID, rm.Name, "done", "")
			}
		}
	}()
}
