// Package gitremote 描述一个远端：地址解析、脱敏、推送授权。
//
// 这个包不再执行任何 git 命令——传输在 internal/gittransport 里实现。
// 留在包里的都是「与传输无关的事实」：URL 长什么样、哪些分支允许推。
//
// 三条不可让步的规则：
//  1. 绝不强推。记忆是只追加的历史，覆盖远端等于抹掉别人写过的东西。
//  2. 绝不隐式提交/合并。搬运的只是已经存在的提交。
//  3. 凭证不进 URL。URL 会出现在日志、错误信息、界面里。
package gitremote

import (
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout 是单次网络操作的默认上限。
//
// 网络操作必须有时限：目标主机卡死时，无界等待会让整个调用方
// 永久挂住，而症状是「命令没有返回」——比报错难查得多。
const DefaultTimeout = 90 * time.Second

// Remote 是一个已声明的远端。
type Remote struct {
	// Name 是这个远端的本地别名，例如 origin / github / blackbox。
	Name string
	// URL 是远端地址，不允许内嵌凭证。
	URL string
	// FetchOnly 为真时禁止向它推送。
	FetchOnly bool
	// PushBranches 是允许推送的分支白名单；空表示不允许推送任何分支。
	PushBranches []string
}

// AllowsPush 报告某个分支是否允许推送。
func (r Remote) AllowsPush(branch string) bool {
	if r.FetchOnly {
		return false
	}
	for _, b := range r.PushBranches {
		if b == branch {
			return true
		}
	}
	return false
}

// Redact 去掉地址里的凭证，用于日志与错误信息。
//
// 显式带账号的地址（https://user:token@host/x）必须能安全地出现在
// 任何输出里——否则一次报错就把 token 写进终端历史和日志文件。
func Redact(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
