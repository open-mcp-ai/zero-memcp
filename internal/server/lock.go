package server

import (
	"errors"
	"fmt"
	"strings"
)

// 分支锁：lock / unlock。
//
// 语义只有一句：锁住之后，**非 root 凭证对该分支的读写全部拒绝**。
// root 不受影响——锁住的目的就是让 root 进去处理（分拣、搬运、归档），
// 如果连 root 都进不去，这个分支就没法回收了。
//
// 锁不改任何 token 的能力，也不改配置。它是分支上的一道临时闸门，
// 与「这张凭证本来能做什么」正交：解锁之后，各凭证照旧按自己的
// mount / cap / 配置行事。

// execLock 实现 lock 动词。
//
//	lock <project>@<branch>
func (s *Server) execLock(rest string, c Caller) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	proj, branch, err := s.parseLockAddr(rest)
	if err != nil {
		return "", err
	}
	if err := s.requireRootAccess(c, proj, branch, configBitWrite); err != nil {
		return "", err
	}
	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}
	if locked, at := repo.BranchLocked(branch); locked {
		return fmt.Sprintf("%s@%s 已经处于锁定状态（锁在 %s）", proj, branch, shortSHA(at)), nil
	}
	sha, err := repo.LockBranch(branch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已锁定 %s@%s（锁在 %s）：非 root 凭证对该分支的读写将被拒绝；root 不受影响",
		proj, branch, shortSHA(sha)), nil
}

// execUnlock 实现 unlock 动词。
//
//	unlock <project>@<branch>
func (s *Server) execUnlock(rest string, c Caller) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	proj, branch, err := s.parseLockAddr(rest)
	if err != nil {
		return "", err
	}
	if err := s.requireRootAccess(c, proj, branch, configBitWrite); err != nil {
		return "", err
	}
	repo, err := s.projects.Get(proj)
	if err != nil {
		return "", err
	}
	wasLocked, at := repo.BranchLocked(branch)
	removed, err := repo.UnlockBranch(branch)
	if err != nil {
		return "", err
	}
	if !removed {
		return fmt.Sprintf("%s@%s 本来就没有锁", proj, branch), nil
	}
	// 解锁时对照一下：锁定期间这个分支动过没有。
	//
	// 不是为了拦截，是为了让「处理完了」这句话有依据——
	// 如果它已经不在锁定的那个提交上，说明期间有写入（只可能是 root），
	// 那件事值得被说出来，而不是静静消失。
	msg := fmt.Sprintf("已解锁 %s@%s", proj, branch)
	now, resolveErr := repo.Resolve(branch)
	switch {
	case resolveErr != nil || now == "":
		// 分支在锁定期间被归档/删掉了：锁解了，但这个分支已经不在。
		// 直说，不要谎报「停在原处」。
		msg += "（该分支已不存在——锁定期间被 archive 或 reset 走了）"
	case !wasLocked || at == "":
	case now == at:
		msg += fmt.Sprintf("（仍停在锁定时的 %s，期间没有写入）", shortSHA(at))
	default:
		msg += fmt.Sprintf("（锁定期间从 %s 推进到 %s）", shortSHA(at), shortSHA(now))
	}
	return msg, nil
}

// parseLockAddr 解析 <project>@<branch>，分支必须显式给出。
func (s *Server) parseLockAddr(rest string) (project, branch string, err error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", errors.New("用法：lock|unlock <project>@<branch>")
	}
	if !strings.Contains(rest, "@") {
		return "", "", fmt.Errorf("必须指定分支：%s@<branch>", rest)
	}
	proj, br, _, perr := s.parseBranchAddr(rest)
	if perr != nil {
		return "", "", perr
	}
	if br == "" {
		return "", "", fmt.Errorf("必须指定分支：%s@<branch>", proj)
	}
	return proj, br, nil
}
