package server

import "strings"

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

// splitTreeHeadKey 是 treeHeadKey 的逆运算。
//
// 键里用 \x00 当分隔符，因为项目名本身含斜杠（BlackEggAI/zero），
// 用斜杠或冒号都会和项目名、分支名撞上。
func splitTreeHeadKey(key string) (project, branch string, ok bool) {
	i := strings.IndexByte(key, 0)
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// publishExternalTreeChanges 把「外部推进」的分支变化广播给订阅者。
//
// 与 MCP 写入路径的 publishTreeChange 分工相同：那边 origin 是真实身份，
// 这边只能是 external——外部推进（另一台机器 push、人手 git commit、
// 第三方 bot 直接推仓库）没有可信的身份信息，标成 external 是诚实的做法。
// 客户端据此决定要不要唤醒自己；宁可多醒一次，也不假装知道是谁写的。
func (s *Server) publishExternalTreeChanges(current map[string]branchHead) {
	for key, head := range current {
		prev := ""
		if val, ok := s.lastCommits.Load(key); ok {
			prev = val.(string)
		}
		if prev == head.Commit {
			continue
		}
		proj, branch, ok := splitTreeHeadKey(key)
		if !ok {
			continue
		}
		s.publishTreeChange(proj, branch, "", prev, head.Commit, EventOrigin{
			Identity: "external",
			Endpoint: s.cfg.Endpoint,
		})
	}
}
