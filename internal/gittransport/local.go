package gittransport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 本地路径远端：url 直接指向一个裸仓库目录（备份盘、挂载卷、同机另一个节点）。
//
// 这种情况下没有网络协议可谈，只有两个仓库之间的对象搬运。
// 它比 SSH/HTTP 短路得多，但仍然走同一套规则：只允许快进、只更新
// refs/remotes/<name>/*（fetch）或目标分支（push）。
//
// 已知边界：直接写对方仓库，不参与对方进程的锁与脏层。
// 因此它适合「目标是一份安静的备份」；两个活着的服务互相直连本地路径
// 应当改用 HTTP。

// LocalPath 报告地址是否是本地裸仓库路径。
func LocalPath(remoteURL string) (string, bool) {
	return localRepoPath(remoteURL)
}

func localRepoPath(remoteURL string) (string, bool) {
	raw := strings.TrimSpace(remoteURL)
	if strings.HasPrefix(raw, "file://") {
		return strings.TrimPrefix(raw, "file://"), true
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") {
		return raw, true
	}
	return "", false
}

// FetchLocal 从本地裸仓库取回历史。
func FetchLocal(repo *gitstore.Repo, remoteURL, remote string) (FetchResult, error) {
	path, ok := localRepoPath(remoteURL)
	if !ok {
		return FetchResult{}, fmt.Errorf("不是本地路径远端：%s", remoteURL)
	}
	src, err := gitstore.Open(path)
	if err != nil {
		return FetchResult{}, fmt.Errorf("打开本地远端仓库失败: %w", err)
	}
	refs := map[string]string{}
	var tips []string
	list, err := src.Refs("refs/heads/")
	if err != nil {
		return FetchResult{}, err
	}
	for _, ref := range list {
		sha, err := src.Resolve(ref)
		if err != nil || sha == "" {
			continue
		}
		refs[ref] = sha
		tips = append(tips, sha)
	}
	res := FetchResult{Branches: map[string]string{}}
	if len(tips) == 0 {
		return res, nil
	}
	pack, err := src.BuildPack(tips, nil)
	if err != nil {
		return res, fmt.Errorf("打包本地远端对象失败: %w", err)
	}
	return applyFetch(repo, remote, refs, pack)
}

// ListLocal 只读本地裸仓库的分支表。
func ListLocal(remoteURL string) (map[string]string, error) {
	path, ok := localRepoPath(remoteURL)
	if !ok {
		return nil, fmt.Errorf("不是本地路径远端：%s", remoteURL)
	}
	src, err := gitstore.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开本地远端仓库失败: %w", err)
	}
	refs := map[string]string{}
	list, err := src.Refs("refs/heads/")
	if err != nil {
		return nil, err
	}
	for _, ref := range list {
		if sha, err := src.Resolve(ref); err == nil && sha != "" {
			refs[ref] = sha
		}
	}
	return refs, nil
}

// PushLocal 把一个分支推进本地裸仓库。
func PushLocal(repo *gitstore.Repo, remoteURL, branch string) (PushResult, error) {
	if err := validateBranch(branch); err != nil {
		return PushResult{}, err
	}
	path, ok := localRepoPath(remoteURL)
	if !ok {
		return PushResult{}, fmt.Errorf("不是本地路径远端：%s", remoteURL)
	}
	unlock, err := lockLocalRepo(path, localLockTimeout)
	if err != nil {
		return PushResult{}, err
	}
	defer unlock()
	dst, err := gitstore.Open(path)
	if err != nil {
		return PushResult{}, fmt.Errorf("打开本地远端仓库失败: %w", err)
	}
	refs := map[string]string{}
	if sha, err := dst.Resolve(branch); err == nil && sha != "" {
		refs["refs/heads/"+branch] = sha
	}
	plan, err := planPush(repo, branch, refs)
	if err != nil {
		return PushResult{}, err
	}
	if plan.UpToDate {
		return plan.Result(), nil
	}
	pack, err := repo.BuildPack([]string{plan.New}, []string{plan.Old})
	if err != nil {
		return PushResult{}, fmt.Errorf("打包失败: %w", err)
	}
	if len(pack) == 0 {
		return PushResult{}, errors.New("本地对象库缺少远端尚无的提交对象")
	}
	if _, err := dst.IngestPack(pack); err != nil {
		return PushResult{}, fmt.Errorf("写入本地远端失败: %w", err)
	}
	if err := dst.SetRef("refs/heads/"+branch, plan.New); err != nil {
		return PushResult{}, err
	}
	return plan.Result(), nil
}

// lockLocalRepo 在目标仓库上取一把协作锁。
//
// 本地路径远端是直接写对方的对象库：两个进程同时写同一个裸仓库时，
// 各自写文件是原子的，但「读 ref → 判断快进 → 写 ref」这个序列不是。
// 锁只覆盖这一段，且必须有超时——拿不到就报错，不排队等。
//
// 边界说明：这把锁只协调同样是 zero-memcp 的写入方；
// 外部的 git 进程不会看它（也不会看 git 自己的锁）。本地路径远端
// 本来就只适合「目标是一份安静的备份」。
func lockLocalRepo(path string, timeout time.Duration) (func(), error) {
	lockPath := filepath.Join(path, "zero-memcp.lock.pid")
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("创建仓库锁失败: %w", err)
		}
		// 锁文件属于一个已经不存在的进程：直接接管，否则一次崩溃会永久卡住。
		if staleLock(lockPath) {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("远端仓库正被另一个进程写入（%s）", lockPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// staleLock 判断锁文件是否由已退出的进程留下。
// 读不懂就当它是活的：宁可报「有人在写」，也不要猜错后覆盖别人的写入。
func staleLock(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false
	}
	return !processAlive(pid)
}

// localLockTimeout 是等锁的上限：备份写入是短动作，等太久说明有别的状况。
const localLockTimeout = 5 * time.Second
