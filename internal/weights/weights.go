// Package weights 记录「有哪些记忆真的被读过」，并把这件事变成检索上的加权。
//
// 只统计 read，不统计 recall。
//
// recall 只是列出候选，我瞄一眼没读，说明它没帮上忙。如果连"出现在候选里"都算
// 一次强化，就会出现自我强化回路——常常浮上来的越来越常浮上来，跟重不重要无关。
// read 是明确的"我用了它"，所以只有它算。
package weights

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State 是派生状态，落在服务本地，不进任何仓库。
// 删掉它所有内容都还在，只是"我读过什么"归零。
type State struct {
	mu   sync.Mutex
	path string

	Hits map[string]int   `json:"hits"`
	Last map[string]int64 `json:"last"`

	dirty bool
	timer *time.Timer
}

func Open(path string) (*State, error) {
	s := &State{
		path: path,
		Hits: make(map[string]int),
		Last: make(map[string]int64),
	}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	_ = json.Unmarshal(data, s)
	if s.Hits == nil {
		s.Hits = make(map[string]int)
	}
	if s.Last == nil {
		s.Last = make(map[string]int64)
	}
	return s, nil
}

func (s *State) Count(addr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Hits[addr]
}

func (s *State) Bump(addr string, now int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Hits[addr]++
	s.Last[addr] = now
	s.dirty = true
	s.scheduleFlushLocked()
	return s.Hits[addr]
}

func (s *State) scheduleFlushLocked() {
	if s.path == "" || s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(500*time.Millisecond, func() {
		s.mu.Lock()
		s.timer = nil
		dirty := s.dirty
		s.dirty = false
		s.mu.Unlock()
		if dirty {
			_ = s.Flush()
		}
	})
}

func (s *State) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	snapshot := struct {
		Hits map[string]int   `json:"hits"`
		Last map[string]int64 `json:"last"`
	}{Hits: s.Hits, Last: s.Last}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	s.dirty = false
	return nil
}

func (s *State) Prune(exists func(addr string) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for addr := range s.Hits {
		if !exists(addr) {
			delete(s.Hits, addr)
			delete(s.Last, addr)
			removed++
		}
	}
	if removed > 0 {
		s.dirty = true
	}
	return removed
}

func (s *State) Close() error {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	return s.Flush()
}
