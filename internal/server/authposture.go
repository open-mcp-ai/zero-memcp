package server

import (
	"fmt"
	"net"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// 监听面与鉴权姿态的自洽性。
//
// 背景（2026-11-24 群内评审第 5 条，主人定为安全红线）：
//
//	enforce_auth 默认关闭，本机任何进程不带凭证就能当 root。
//	这在单人笔记本上是方便的，但一旦按 IP 对多台机器提供服务，
//	「本机」就不再等于「可信」——同一台机器上任何能发 TCP 的东西
//	（被攻破的 sidecar、跑偏的脚本、别人的容器）都能读到全部记忆、
//	写进任意分支。
//
// 注意这**不是**「对外网暴露」的问题：非回环来源本来就必须带凭证
// （authenticate 里 /repos/ 与信箱一律要 token，其余走 token 分支）。
// 漏的正是回环那一条——服务监听在非回环地址上，说明这台机器不是
// 单机自用，那它就不该再给本机进程开免签的后门。
//
// 端口级 require = "bearer" 是等价的收窄手段，认它。
// 两个都没给就直接拒绝启动：配置写错必须在启动那一刻出声，
// 而不是安静地以「谁都能当 root」的姿态跑上几个月。

// checkAuthPosture 判定监听面与鉴权姿态是否自相矛盾。
//
// listen 是生效的主监听地址，blocks 是 [[listen]] 块。
func checkAuthPosture(listen string, enforceAuth bool, blocks []config.ListenBlock) error {
	if enforceAuth {
		return nil
	}
	if listen != "" && !isLoopbackListen(listen) {
		return fmt.Errorf(
			"refusing to start: listening on %q (reachable over the network) while enforce_auth = false.\n"+
				"  Any process on this host could reach 127.0.0.1 and act as root, and this host is not single-user anymore.\n"+
				"  Fix: set enforce_auth = true in [server] (and give every local client its own credential).\n"+
				"  See deploy notes: multi-machine setups must turn this on.",
			listen)
	}
	for _, b := range blocks {
		if b.Addr == "" || isLoopbackListen(b.Addr) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(b.Require), "bearer") {
			continue
		}
		return fmt.Errorf(
			"refusing to start: [[listen]] addr = %q (reachable over the network) is not protected.\n"+
				"  Fix one of: enforce_auth = true in [server], or require = \"bearer\" on this [[listen]] block.\n"+
				"  Without it, any local process can act as root on this port without a credential.",
			b.Addr)
	}
	return nil
}

// authPosture 是给状态输出用的一句话：这个端口到底怎么认人。
func authPosture(addr string, enforceAuth bool) string {
	if enforceAuth {
		return "enforced"
	}
	if isLoopbackListen(addr) {
		return "loopback-exempt"
	}
	// 走到这里说明启动校验被绕过了（例如运行中 Reload 换了监听面）。
	// 说出来，别装作没问题。
	return "OPEN: reachable over the network and enforce_auth is off"
}

// isLoopbackListen 判断一个监听地址是否只对本机可见。
//
// 判据只看主机部分，不看端口：
//
//	127.0.0.1:1729   → 回环
//	localhost:1729   → 回环
//	[::1]:1729       → 回环
//	:1729            → 全网卡（空主机名 = 所有地址）
//	0.0.0.0:1729     → 全网卡
//	[::]:1729        → 全网卡
//	10.10.10.12:1729 → 具体网卡，可被网络访问
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		// 没有端口段的形态（例如 ":1729" 会被正常解析，裸 "127.0.0.1" 不会）：
		// 当成主机名再判一次；判不出来一律按「非回环」处理，宁可拒绝启动。
		host = strings.TrimSpace(addr)
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		// 空主机名 = 绑定所有地址。
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// 解析不出 IP 的主机名无法证明它只对本机可见。
		return false
	}
	return ip.IsLoopback()
}
