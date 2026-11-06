package server

// 分支头水位：记录本进程上一次看到的各分支 HEAD。
//
// 它存在的唯一理由是发现**外部提交**：另一台机器 push 上来、
// 或者人手敲了 git commit，进程需要知道「文件系统变了」并触发 Reload。
// 这部分是必需的，与推送无关。
//
// 这里原本还有一整套 SSE 广播（remind 锚定 → tree 事件 → 长连接推送）。
// 那一层已移除：它自造了一套协议，而本服务的对外契约是 MCP。
// 变更感知改由调用方主动查询（`status` 报告关注的树，`since` 取差量），
// 全部走 term 动词，不再有第二条通道。

// branchHead 是一个项目分支当前可观察到的提交。
type branchHead struct {
	Project string
	Branch  string
	Commit  string
}

func treeHeadKey(project, branch string) string { return project + "\x00" + branch }

// treeHeadsSnapshot 读出本机全部项目的当前 heads。
func (s *Server) treeHeadsSnapshot() map[string]branchHead {
	out := make(map[string]branchHead)
	projects, err := s.projects.Projects()
	if err != nil {
		return out
	}
	for _, project := range projects {
		repo, err := s.projects.Get(project)
		if err != nil {
			continue
		}
		for _, branch := range repo.Branches() {
			sha, err := repo.Resolve(branch)
			if err != nil || sha == "" {
				continue
			}
			out[treeHeadKey(project, branch)] = branchHead{Project: project, Branch: branch, Commit: sha}
		}
	}
	return out
}

// rememberTreeHeads 用当前文件系统状态重建水位。
func (s *Server) rememberTreeHeads() {
	current := s.treeHeadsSnapshot()
	alive := make(map[string]bool, len(current))
	for key, head := range current {
		alive[key] = true
		s.lastCommits.Store(key, head.Commit)
	}
	s.lastCommits.Range(func(key, _ any) bool {
		if k, ok := key.(string); ok && !alive[k] {
			s.lastCommits.Delete(k)
		}
		return true
	})
}
