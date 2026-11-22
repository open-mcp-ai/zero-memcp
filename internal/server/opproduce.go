package server

// 命令流的本地生产侧：把每一次树变更登记成命令。
//
// 原则：**命令流是唯一真相**。write/delete/mv/cp 在这里留下位点，
// 对端同步时发的就是这同一份日志——不存在「同步专用」的第二套数据。

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

// noteWriteOp 记录一次内容写入。
func (s *Server) noteWriteOp(project, branch, path, mode, sha string) {
	if s.opLogs == nil || sha == "" {
		return
	}
	if _, err := s.recordTreeOp(project, branch, opSet, path, "", mode, sha); err != nil {
		// 登记失败必须出声：静默失败意味着这次改动永远不会被同步，
		// 而本地看起来一切正常。
		s.logf("oplog: record set %s@%s:%s failed: %v", project, branch, path, err)
		return
	}
	s.kickSync()
}

// noteDeleteOp 记录一次删除。删除是一条命令，不是特例。
func (s *Server) noteDeleteOp(project, branch, path string) {
	if s.opLogs == nil {
		return
	}
	if _, err := s.recordTreeOp(project, branch, opDelete, path, "", "", ""); err != nil {
		s.logf("oplog: record delete %s@%s:%s failed: %v", project, branch, path, err)
		return
	}
	s.kickSync()
}

// noteMoveOp 记录一次原子改名。
func (s *Server) noteMoveOp(project, branch, from, to string) {
	if s.opLogs == nil {
		return
	}
	if _, err := s.recordTreeOp(project, branch, opRename, to, from, "", ""); err != nil {
		s.logf("oplog: record rename %s@%s:%s -> %s failed: %v", project, branch, from, to, err)
		return
	}
	s.kickSync()
}

// noteCopyOp 记录一次原子复制。
func (s *Server) noteCopyOp(project, branch, from, to, mode, sha string) {
	if s.opLogs == nil {
		return
	}
	if _, err := s.recordTreeOp(project, branch, opCopy, to, from, mode, sha); err != nil {
		s.logf("oplog: record copy %s@%s:%s -> %s failed: %v", project, branch, from, to, err)
		return
	}
	s.kickSync()
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
