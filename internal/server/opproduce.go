package server

// 命令流的本地生产侧：把每一次树变更登记成命令。
//
// 原则：**命令流是唯一真相**。write/delete/mv/cp 在这里留下位点，
// 对端同步时发的就是这同一份日志——不存在「同步专用」的第二套数据。
//
// # 为什么广播也放在这里（2026-11-26）
//
// 一次本地写入有两个不可省略的副作用：
//
//	记命令流   —— 不记，这次改动永远不同步，对端永远看不到；
//	广播变更   —— 不广播，订阅了这棵树的分身永远收不到通知。
//
// 两者早先分散在各个动词里，由每个写站点「记得各调一次」。实测结果：
//
//	put    ✓ 两个都调
//	write  ✓ 但曾漏了 noteWriteOp（同步凭空丢改动），后又漏了广播
//	apply  ✗ 从来没有广播
//	cp/mv  ✗ 从来没有广播
//	ref    ✗ 从来不记命令流（内容永远不同步）
//
// 同一类缺口在同一个文件里栽过两次，说明「靠每个站点记得调」这个结构
// 本身是错的。现在收口成一条：**调 note*Op 就等于做完这两件事**，
// 想漏也漏不掉。

// nodeIdentity 给出本节点在命令流里的名字。
//
// 与 instanceID 分开：后者随监听地址变，而日志目录必须稳定——
// 否则改一次端口，历史命令就「属于另一个节点」，位点全部重来。
func nodeIdentity(cfg Config) string {
	if cfg.Endpoint != "" {
		return cfg.Endpoint
	}
	return "local"
}

// publishLocalWrite 广播一次本地写入。
//
// 与对端落地路径（opapply.go / syncwork.go）的区别只在 origin：
// 那边是 `peer:<节点名>`，这边是写入者自己的身份。客户端据此区分
// 「我自己写的」与「别人写的」，自激循环是否成立交给客户端策略。
func (s *Server) publishLocalWrite(project, branch, path string, c Caller) {
	s.publishTreeChange(project, branch, path, "", "", EventOrigin{
		Identity: c.Identity,
		Session:  c.Session,
		Endpoint: s.cfg.Endpoint,
	})
}

// noteWriteOp 记录一次内容写入，并广播。
//
// c 是写入者身份，用于事件的 origin。传零值 Caller 时退化为
// 「本机某个未具名写入者」——仍然广播，因为「谁写的」不确定不是
// 不广播的理由（订阅方少收一次通知，比标错身份更糟）。
func (s *Server) noteWriteOp(c Caller, project, branch, path, mode, sha string) {
	if s.opLogs == nil || sha == "" {
		// 没有命令流时仍然要广播：本地订阅者关心的是内容变了没有，
		// 不是这次改动能不能同步。
		s.publishLocalWrite(project, branch, path, c)
		return
	}
	if _, err := s.recordTreeOp(project, branch, opSet, path, "", mode, sha); err != nil {
		// 登记失败必须出声：静默失败意味着这次改动永远不会被同步，
		// 而本地看起来一切正常。
		s.logf("oplog: record set %s@%s:%s failed: %v", project, branch, path, err)
		// 广播照旧：同步断了不该连带让订阅者失明。
		s.publishLocalWrite(project, branch, path, c)
		return
	}
	s.kickSync()
	s.publishLocalWrite(project, branch, path, c)
}

// noteDeleteOp 记录一次删除，并广播。删除是一条命令，不是特例。
func (s *Server) noteDeleteOp(c Caller, project, branch, path string) {
	if s.opLogs != nil {
		if _, err := s.recordTreeOp(project, branch, opDelete, path, "", "", ""); err != nil {
			s.logf("oplog: record delete %s@%s:%s failed: %v", project, branch, path, err)
		} else {
			s.kickSync()
		}
	}
	s.publishLocalWrite(project, branch, path, c)
}

// noteMoveOp 记录一次原子改名，并广播（源与目标都算变更）。
func (s *Server) noteMoveOp(c Caller, project, branch, from, to string) {
	if s.opLogs != nil {
		if _, err := s.recordTreeOp(project, branch, opRename, to, from, "", ""); err != nil {
			s.logf("oplog: record rename %s@%s:%s -> %s failed: %v", project, branch, from, to, err)
		} else {
			s.kickSync()
		}
	}
	s.publishLocalWrite(project, branch, from, c)
	s.publishLocalWrite(project, branch, to, c)
}

// noteCopyOp 记录一次原子复制，并广播。
func (s *Server) noteCopyOp(c Caller, project, branch, from, to, mode, sha string) {
	if s.opLogs != nil {
		if _, err := s.recordTreeOp(project, branch, opCopy, to, from, mode, sha); err != nil {
			s.logf("oplog: record copy %s@%s:%s -> %s failed: %v", project, branch, from, to, err)
		} else {
			s.kickSync()
		}
	}
	s.publishLocalWrite(project, branch, to, c)
}

// noteTreeOp 把一棵完整的树登记成一条批量命令。
//
// 这是历史层整树同步进入命令流的入口：一棵对端没有的树，
// 在这里就是一批节点添加。
func (s *Server) noteTreeOp(project, branch string) error {
	entries, err := s.flattenTreeToOps(project, branch)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	_, err = s.recordTreeBatch(project, branch, entries)
	return err
}
