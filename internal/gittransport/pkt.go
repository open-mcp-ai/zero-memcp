package gittransport

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// pkt-line 是 Git 传输层的帧格式：4 字节十六进制长度 + 内容。
// 长度 0 是 flush，1 是分隔符，2 是响应结束——后两者只在协议 v2 出现。

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

func readPkt(r *bufio.Reader) (string, pktKind, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return "", pktData, err
	}
	var n int
	if _, err := fmt.Sscanf(string(header[:]), "%04x", &n); err != nil {
		return "", pktData, fmt.Errorf("非法 pkt-line header %q", header)
	}
	switch n {
	case 0:
		return "", pktFlush, nil
	case 1:
		return "", pktDelim, nil
	case 2:
		return "", pktResponseEnd, nil
	}
	if n < 4 || n > 65520 {
		return "", pktData, fmt.Errorf("pkt-line 长度越界: %d", n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", pktData, err
	}
	return string(payload), pktData, nil
}

// readAdvertisementProtocol 判断服务端用的是哪个协议版本。
//
// v2 的广告第一行就是 "version 2"，之后是能力清单而不是 ref。
// 这个判断必须放在读 ref 之前：把能力行当 ref 解析会得到一堆
// 形如 "fetch=shallow" 的假分支。
func readAdvertisementProtocol(r *bufio.Reader) (int, error) {
	line, kind, err := readPkt(r)
	if err != nil {
		return 0, err
	}
	if kind == pktData {
		trimmed := strings.TrimRight(line, "\n")
		if trimmed == "version 2" {
			return 2, nil
		}
	}
	// 不是 v2：把这一帧放回去，交给 v0 的广告读取器。
	return 0, errNotV2
}

var errNotV2 = fmt.Errorf("服务端未使用协议 v2")

// v2Caps 解析 v2 能力广告，读到 flush 为止。
func v2Caps(r *bufio.Reader) (map[string]string, error) {
	caps := map[string]string{}
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			return nil, err
		}
		if kind == pktFlush {
			return caps, nil
		}
		if kind != pktData {
			continue
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		caps[key] = value
	}
}

// v2LsRefsRequest 请求 refs/heads/* 下的全部引用。
//
// 只要 ref-prefix heads：记忆库的分支全在 heads 下，把 tags/notes 也拉回来
// 只会让人误以为它们也是同步目标。
func v2LsRefsRequest() []byte {
	var b []byte
	b = append(b, pktLine("command=ls-refs\n")...)
	b = append(b, pktLine("agent=zero-memcp\n")...)
	b = append(b, delimPkt...)
	b = append(b, pktLine("ref-prefix refs/heads/\n")...)
	b = append(b, flushPkt...)
	return b
}

func parseV2LsRefs(r *bufio.Reader) (map[string]string, error) {
	refs := map[string]string{}
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			if err == io.EOF {
				return refs, nil
			}
			return nil, err
		}
		if kind == pktFlush || kind == pktResponseEnd {
			return refs, nil
		}
		if kind != pktData {
			continue
		}
		fields := strings.Fields(strings.TrimRight(line, "\n"))
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(fields[1], "refs/") && validSHA(fields[0]) {
			refs[fields[1]] = fields[0]
		}
	}
}

// v2FetchRequest 组装 fetch 命令：要哪些 tip，我有这些，然后就这些。
func v2FetchRequest(tips, haves []string) []byte {
	var b []byte
	b = append(b, pktLine("command=fetch\n")...)
	b = append(b, pktLine("agent=zero-memcp\n")...)
	b = append(b, delimPkt...)
	for _, sha := range tips {
		b = append(b, pktLine("want "+sha+"\n")...)
	}
	for _, sha := range haves {
		b = append(b, pktLine("have "+sha+"\n")...)
	}
	b = append(b, pktLine("done\n")...)
	b = append(b, flushPkt...)
	return b
}

// parseV2Pack 跳过 ack 段，取出 packfile 段的字节。
//
// v2 的响应是「段」结构：acknowledgments / shallow-info / wanted-refs / packfile，
// 段之间用分隔符隔开。packfile 段头之后就是原始 pack 数据，不再是 pkt-line。
func parseV2Pack(r *bufio.Reader) ([]byte, error) {
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			return nil, fmt.Errorf("读取 v2 响应失败: %w", err)
		}
		switch kind {
		case pktFlush, pktDelim, pktResponseEnd:
			continue
		}
		trimmed := strings.TrimRight(line, "\n")
		if strings.HasPrefix(trimmed, "ERR ") {
			return nil, fmt.Errorf("远端拒绝取回: %s", strings.TrimPrefix(trimmed, "ERR "))
		}
		switch trimmed {
		case "packfile":
			return readSidebandPack(r)
		case "acknowledgments", "shallow-info", "wanted-refs":
			// 段的头行：内容本身不需要，逐行读到下一个分隔符。
			continue
		}
		// ack 段里的 NAK / ACK 行，或 wanted-refs 的 <oid> <ref> 行。
	}
}

var (
	flushPkt = []byte("0000")
	delimPkt = []byte("0001")
	emptyPkt = []byte("0004")
)

// readSidebandPack 解复用 packfile 段。
//
// 协议 v2 的 packfile 段**总是**走 side-band-64k：每个 pkt-line 的首字节是通道号，
// 1 是 pack 数据，2 是进度信息，3 是错误。直接把段尾当成原始 pack 读取，
// 拿到的会是带进度文本的 pkt-line——症状是「缺 PACK 头」，而真正的 pack
// 就藏在 0x01 通道里。
func readSidebandPack(r *bufio.Reader) ([]byte, error) {
	var pack []byte
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("读取 pack 分片失败: %w", err)
		}
		if kind != pktData {
			if kind == pktResponseEnd || kind == pktFlush {
				break
			}
			continue
		}
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case 1:
			pack = append(pack, line[1:]...)
		case 2:
			// 进度信息：丢弃，但不清空——它不带 pack 内容。
			continue
		case 3:
			return nil, fmt.Errorf("远端在传输中报错: %s", strings.TrimSpace(line[1:]))
		default:
			// 没有通道字节：某些实现直接发裸 pack 数据。
			pack = append(pack, line...)
		}
		if len(pack) > maxAdvertisement {
			return nil, fmt.Errorf("pack 超过 %d 字节上限", maxAdvertisement)
		}
	}
	if len(pack) == 0 {
		return nil, fmt.Errorf("远端返回了空的 packfile 段")
	}
	return pack, nil
}
