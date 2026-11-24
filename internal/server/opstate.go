package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
)

// 同步水位的落盘。
//
// 为什么必须落盘（2026-11-22 真双实例狗粮实测）：
//
//	水位原本只在内存里。进程一重启，in 归零，下一轮就把对端整份历史
//	重新拉一遍——命令幂等（applied 0，不会破坏数据），但每条路径都要
//	走一次 refreshPath + publishTreeChange。分支只有 80 个文件时是
//	3 秒的噪音；真实分支上几千个文档时，就是每次重启白烧一遍全量索引，
//	而且日志里看不出任何错误。
//
// 与旧实现（work-layer 快照）的差别：那时没有水位这个概念，靠
// dropAlreadyInSync 逐条比对。命令流的位点本身就是水位，落盘即够。

// opSyncState 是落盘的同步状态。
type opSyncState struct {
	// Out 是「对端已确认收到我本地日志的哪一位」，键为 project\x00branch\x00peer。
	Out map[string]int64 `json:"out"`
	// In 是「我已同步到对端日志的哪一位」。
	In map[string]int64 `json:"in"`
	// Aliases 是「配置里的对端别名 → 对端自报的日志属主名」。
	//
	// 必须一起落盘：水位键用的是属主名，而重启后本地只剩配置里的别名。
	// 不存这张表，重启后按别名查水位会得 0，于是又从位点 0 拉一遍——
	// 正是这个文件要消灭的行为（2026-11-22 测试抓到的第二层坑）。
	Aliases map[string]string `json:"aliases,omitempty"`
	// Bases 是「该对端的水位属于哪段命令流」。
	//
	// 命令流提交即归零，位点只在同一段流内可比。只存数字会出现最坏的
	// 对齐：旧流 seq 1 与新流 seq 1 数字相同、含义不同，Since(1) 返回空，
	// 同步静默停摆。所以水位必须连基准一起存。
	Bases map[string]string `json:"bases,omitempty"`
	// PeerTrees 记录各对端手上的内容树。重启后不能丢：
	// 丢了下一轮就会判成「不知道对端有什么」而重发整棵树。
	PeerTrees map[string]string `json:"peer_trees,omitempty"`
	// Blocked 是「正文在本地拿不到」的命令留证。
	//
	// 必须落盘（群内评审第 2 条同类问题）：它是「同步为什么没走完」的唯一
	// 证据，重启就失的话，一次重启就把一个正在挂着的故障变成不可见。
	Blocked []blockedBlob `json:"blocked,omitempty"`
	// WorkConflicts 是工作层的待裁决冲突。
	//
	// 必须落盘（群内评审第 2 条）：早先它只在内存里，重启后 status 不再报，
	// 但冲突还躺在盘上——本地那份没被覆盖、远端那份也在对象库，就是没人
	// 知道还欠着裁决。一次重启把一个待办事项变成不存在。
	WorkConflicts []workConflictRecord `json:"work_conflicts,omitempty"`
	// GitConflicts 是 Git 层（分支级）的待裁决冲突。
	GitConflicts []syncConflict `json:"git_conflicts,omitempty"`
	// DeleteEvicted 是累计被淘汰的删除留证条数。
	//
	// 这个数字必须落盘：它是对「有多少证据已经不在内存里」的唯一交代。
	// 归零会让 status 假报「一条都没丢」。
	DeleteEvicted int64 `json:"delete_evicted,omitempty"`
}

// opSyncStatePath 返回水位文件的路径。
func (s *Server) opSyncStatePath() string {
	return filepath.Join(s.cfg.DataDir, "system", "opsync.json")
}

// loadOpSyncState 在启动时恢复水位。
//
// 文件不存在不是错误：新节点本来就没有水位。
// 文件损坏则报错而不是静默归零——静默归零会退化成「每次重启全量重拉」，
// 这正是这个文件要消灭的行为。
func (s *Server) loadOpSyncState() error {
	if s.workSync == nil {
		return nil
	}
	path := s.opSyncStatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var st opSyncState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	for k, v := range st.Out {
		noteWatermark(&s.workSync.out, k, v)
	}
	for k, v := range st.In {
		noteWatermark(&s.workSync.in, k, v)
	}
	for k, v := range st.Bases {
		s.workSync.bases.Store(k, v)
	}
	for k, v := range st.PeerTrees {
		s.workSync.peerTrees.Store(k, v)
	}
	// 缺正文留证一并恢复：一次重启不能把一个正挂着的故障变成不可见。
	for _, b := range st.Blocked {
		s.workSync.blocked.Store(blockedKey(b.Project, b.Branch, b.Peer, b.SHA), b)
	}
	// 冲突一并恢复：重启不该让「还欠一次裁决」这件事消失。
	for _, c := range st.WorkConflicts {
		s.workSync.conflicts.Store(workConflictKey(c.Project, c.Branch, c.Peer, c.Path), c.workConflict)
	}
	for _, c := range st.GitConflicts {
		s.syncState.conflicts.Store(syncConflictKey(c), c)
	}
	if st.DeleteEvicted > 0 {
		s.workSync.deleteEvicted.Store(st.DeleteEvicted)
	}
	// 别名先于水位生效：水位查询要靠它把配置别名翻成属主名。
	for alias, node := range st.Aliases {
		s.learnPeerIdentity(alias, node)
	}
	return nil
}

// saveOpSyncState 把当前水位写盘。
//
// 先写临时文件再 rename：直接覆盖时若在写一半崩溃，文件会变成半截
// JSON，下次启动解析失败。rename 在 POSIX 与 Windows 上都是原子的。
func (s *Server) saveOpSyncState() error {
	if s.workSync == nil {
		return nil
	}
	done, err := tokenAcquire(s.watermarkMu, "opsync-state")
	if err != nil {
		return err
	}
	defer done()

	st := opSyncState{Out: map[string]int64{}, In: map[string]int64{}, Aliases: map[string]string{}, Bases: map[string]string{}}
	s.workSync.out.Range(func(k, v any) bool {
		st.Out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	s.workSync.in.Range(func(k, v any) bool {
		st.In[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})

	s.peerAliases.Range(func(k, v any) bool {
		st.Aliases[k.(string)] = v.(string)
		return true
	})

	st.PeerTrees = map[string]string{}
	s.workSync.peerTrees.Range(func(k, v any) bool {
		st.PeerTrees[k.(string)] = v.(string)
		return true
	})
	s.workSync.bases.Range(func(k, v any) bool {
		st.Bases[k.(string)] = v.(string)
		return true
	})
	st.Blocked = s.workSync.blockedAll()
	st.WorkConflicts = s.workConflictsRecorded()
	st.GitConflicts = s.SyncConflicts()
	st.DeleteEvicted = s.workSync.deleteEvicted.Load()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := s.opSyncStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// watermarkKeys 返回当前全部水位键（按字典序），供 status 与测试使用。
func (s *Server) watermarkKeys() []string {
	if s.workSync == nil {
		return nil
	}
	seen := map[string]bool{}
	s.workSync.out.Range(func(k, _ any) bool { seen[k.(string)] = true; return true })
	s.workSync.in.Range(func(k, _ any) bool { seen[k.(string)] = true; return true })
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
