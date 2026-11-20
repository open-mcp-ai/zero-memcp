package server

import (
	"fmt"
	"sync/atomic"
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

// fanoutHub 保存最近几批扇出记录。
//
// 两条纪律，缺一不可：
//
//  1. **外部结构用原子快照**：batches 换成 atomic.Pointer，读者拿到的
//     那份切片永远不被修改（写者造新的再换进去）。
//  2. **批次本身也必须不可变**：FanoutBatch 里有一个 Targets map。
//     如果 updateTarget 原地改这个 map，而 status 正在遍历它，
//     就是 concurrent map iteration and map write —— runtime 直接杀进程。
//     所以更新时整批克隆，连同 Targets 一起复制。
//
// 只换外层不换内层，是这类 bug 最常见的写法：看起来"已经无锁了"，
// 实际上读写仍在同一张 map 上撞。
type fanoutHub struct {
	state atomic.Pointer[[]*FanoutBatch]
	wmu   chan struct{}
}

func newFanoutHub() *fanoutHub {
	h := &fanoutHub{wmu: make(chan struct{}, 1)}
	h.state.Store(&[]*FanoutBatch{})
	return h
}

func (h *fanoutHub) load() []*FanoutBatch {
	if p := h.state.Load(); p != nil {
		return *p
	}
	return nil
}

func (h *fanoutHub) store(b []*FanoutBatch) { h.state.Store(&b) }

func (h *fanoutHub) add(b *FanoutBatch) error {
	done, err := tokenAcquire(h.wmu, "fanout.add")
	if err != nil {
		return err
	}
	defer done()

	cur := h.load()
	next := make([]*FanoutBatch, 0, len(cur)+1)
	next = append(next, b)
	next = append(next, cur...)
	if len(next) > 5 {
		next = next[:5]
	}
	h.store(next)
	return nil
}

func (h *fanoutHub) updateTarget(batchID, remoteName, status, errStr string) {
	done, err := tokenAcquire(h.wmu, "fanout.updateTarget")
	if err != nil {
		return
	}
	defer done()

	cur := h.load()
	for i, b := range cur {
		if b.ID != batchID {
			continue
		}
		// 整批克隆：绝不改 cur 里那份（status 可能正在遍历它的 Targets）。
		clone := *b
		targets := make(map[string]FanoutTarget, len(b.Targets)+1)
		for k, v := range b.Targets {
			targets[k] = v
		}
		targets[remoteName] = FanoutTarget{Status: status, At: time.Now(), Err: errStr}
		clone.Targets = targets

		allDone := true
		for _, t := range targets {
			if t.Status == "pending" || t.Status == "running" {
				allDone = false
				break
			}
		}
		if allDone {
			clone.Completed = true
			clone.DoneAt = time.Now()
		}

		next := make([]*FanoutBatch, len(cur))
		copy(next, cur)
		next[i] = &clone
		h.store(next)
		return
	}
}

// activeOrRecent 返回正在进行中、或完成不到 1 分钟的批次。
//
// 返回的指针指向不可变批次，调用方可以放心读。
func (h *fanoutHub) activeOrRecent() []*FanoutBatch {
	var out []*FanoutBatch
	now := time.Now()
	for _, b := range h.load() {
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
	_ = s.fanouts.add(batch)

	go func() {
		for _, rm := range targets {
			s.fanouts.updateTarget(batch.ID, rm.Name, "running", "")
			// 复用已有的 syncOnce 逻辑
			s.syncOnce(project, rm)
			// 根据 lastErr 确认是否出现错误
			key := project + "@" + rm.Name
			errStr := ""
			if v, ok := s.syncState.lastErr.Load(key); ok {
				errStr, _ = v.(string)
			}

			if errStr != "" {
				s.fanouts.updateTarget(batch.ID, rm.Name, "failed", errStr)
			} else {
				s.fanouts.updateTarget(batch.ID, rm.Name, "done", "")
			}
		}
	}()
}
