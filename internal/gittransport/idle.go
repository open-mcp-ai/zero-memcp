package gittransport

import (
	"context"
	"net"
	"net/http"
	"time"
)

// 流式活跃检测：判据是「字节还在不在流动」，不是「这一轮花了多久」。
//
// 为什么整轮时间预算一定是错的（2026-11-24 3060 冷启动实测）：
//
//	weft 的历史在 3060 上冷页缓存下要读 90 秒以上，而结果是「一个对象都不用发」。
//	给它 90 秒预算，它超时；给它 15 分钟预算，它过了，但下一次更大一点的历史
//	又会超时——预算是一个永远追不上数据增长的常数。
//
//	反过来，真正卡死的那次（8 小时静默停摆）从来不是因为「慢」，
//	而是因为「再也不说话了」。这两件事必须分开：慢但一直有字节 = 正常；
//	很久一个字节都没有 = 死连接。
//
// 实现方式是滑动窗口：每一个读/写动作都把最后期限往后推。
// 只要还有字节流动，请求就能一直跑下去；一旦连续 idle 时长没有任何动作，
// 连接被掐断，调用方拿到一个明确的超时错误。
//
// 这条判据同时覆盖三个阶段，因为它们共用一个 TCP 连接：
//
//	上传请求体（本地打好的 pack，可能有几百 MB）
//	等待响应头（服务端在算 pack，可能很久）
//	读取响应体（70 MB 的 pack 传回来）
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Write(p)
}

// stallTransport 造一个用「连续多久没有字节」当唯一超时判据的 HTTP 传输。
//
// 不设 http.Client.Timeout（那是整请求预算），不设 ResponseHeaderTimeout
// （那会在服务端算包期间把连接掐掉——3060 正是这样：响应头早就发了，
// 包却要 90 秒后才有第一个字节）。
//
// idle <= 0 时退回一个保守默认值（5 分钟）：这个值的含义是
// 「允许服务端安静多久」——对端算一个整仓库的 pack、或者在机械盘上遍历
// 十万个对象，都属于「安静但活着」。
func stallTransport(idle time.Duration) *http.Transport {
	if idle <= 0 {
		idle = DefaultStallTimeout
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &idleConn{Conn: conn, idle: idle}, nil
		},
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// 池里空闲连接单独给一个短命期，和请求期判据解耦。
		//
		// 为什么不能直接沿用 idle：连接回到池子时还带着上次读设的期限，
		// 若它比连接池自己的淘汰更晚到点，下一次复用它就会立刻报超时。
		// 让池先于期限淘汰（30s < idle），这个窗口就不存在。
		IdleConnTimeout: 30 * time.Second,
	}
}

// DefaultStallTimeout 是「允许对端安静多久」的默认值。
//
// 5 分钟不是拍出来的：3060 冷页缓存下算一次全量闭包实测 90~120 秒，
// 加上 Windows 机械盘的抖动，留了约 3 倍余量。它比原来那个 90 秒整轮预算
// 宽，但比「无限等」安全——真正卡死的连接会在 5 分钟内被掐断并报错，
// 而不是像 2026-11-23 那次一样静默挂 8 小时。
const DefaultStallTimeout = 5 * time.Minute
