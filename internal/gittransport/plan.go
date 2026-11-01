package gittransport

import (
	"errors"
	"fmt"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// pushPlan 是一次 push 的判定结果：从哪个 sha 推进到哪个 sha。
type pushPlan struct {
	Branch   string
	Old      string // 远端现有值；新建分支为全零
	New      string
	UpToDate bool // 远端已经等于本地
}

// Result 转成对外的推送结果。
func (p pushPlan) Result() PushResult {
	return PushResult{Branch: p.Branch, Old: p.Old, New: p.New}
}

// planPush 决定这次推送要做什么，并在做之前拒绝一切会改写历史的动作。
//
// 三条判断，缺一不可：
//  1. 本地分支必须存在——推一个不存在的分支只会得到远端的模糊报错；
//  2. 远端已存在该分支时，那边的 sha 必须是本地新 sha 的祖先（快进）；
//  3. 完全相同就什么都不做，但也要如实报告「已是最新」。
func planPush(repo *gitstore.Repo, branch string, refs map[string]string) (pushPlan, error) {
	newSHA, err := repo.Resolve(branch)
	if err != nil || newSHA == "" {
		return pushPlan{}, fmt.Errorf("本地分支 %q 不存在或没有提交", branch)
	}
	oldSHA := refs["refs/heads/"+branch]
	if oldSHA == "" {
		oldSHA = zeroSHA
	}
	plan := pushPlan{Branch: branch, Old: oldSHA, New: newSHA}
	if oldSHA == newSHA {
		plan.UpToDate = true
		return plan, nil
	}
	if oldSHA == zeroSHA {
		return plan, nil
	}
	ok, err := repo.IsAncestor(oldSHA, newSHA)
	if err != nil {
		return pushPlan{}, fmt.Errorf("无法验证是否快进（先 fetch 远端历史）：%w", err)
	}
	if !ok {
		return pushPlan{}, fmt.Errorf("拒绝非快进 push：远端 %s 不是本地 %s 的祖先", oldSHA, newSHA)
	}
	return plan, nil
}

// pushRequest 是一份待发送的 receive-pack 请求体：命令区 + pack。
type pushRequest struct {
	repo   *gitstore.Repo
	branch string
	oldSHA string
	newSHA string
}

// body 组装 pkt-line 命令与 pack 数据。
//
// 只声明 report-status：能力协商要跟着远端广告走，多声明一个自己实现不了的
// 能力（例如 side-band-64k）会让远端按那个格式回包，而客户端读不懂。
func (r pushRequest) body() ([]byte, error) {
	pack, err := r.repo.BuildPack([]string{r.newSHA}, []string{r.oldSHA})
	if err != nil {
		return nil, fmt.Errorf("构建 push pack 失败: %w", err)
	}
	if len(pack) == 0 {
		return nil, errors.New("本地对象库缺少远端尚无的提交对象")
	}
	var buf []byte
	buf = append(buf, pktLine(fmt.Sprintf("%s %s refs/heads/%s\x00report-status\n", r.oldSHA, r.newSHA, r.branch))...)
	buf = append(buf, []byte("0000")...)
	buf = append(buf, pack...)
	return buf, nil
}
