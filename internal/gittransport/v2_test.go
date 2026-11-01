package gittransport

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// v2 的 packfile 段总是 side-band-64k：只有 0x01 通道是数据，
// 0x02 是进度。把进度当 pack 读，症状是「缺 PACK 头」。
func TestSidebandPackDemux(t *testing.T) {
	var buf bytes.Buffer
	// 0x02 进度分片
	buf.Write(pktLine("\x02远端计数: 3\n"))
	// 0x01 数据分片，故意拆成两段
	buf.Write(pktLine("\x01PACK"))
	buf.Write(pktLine("\x01\x00\x00\x00\x02"))
	buf.WriteString("0000")

	pack, err := readSidebandPack(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("解复用失败: %v", err)
	}
	if string(pack) != "PACK\x00\x00\x00\x02" {
		t.Fatalf("pack 内容不符: %q", string(pack))
	}
}

func TestSidebandPackReportsRemoteError(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(pktLine("\x03远端出错：没有权限\n"))
	buf.WriteString("0000")
	if _, err := readSidebandPack(bufio.NewReader(&buf)); err == nil || !strings.Contains(err.Error(), "没有权限") {
		t.Fatalf("应把通道 3 的错误转成错误信息: %v", err)
	}
}

// v0 与 v2 的广告必须被区分开：把 v2 的能力行当成 ref 会造出假分支。
func TestAdvertisementDetectsProtocolVersion(t *testing.T) {
	var v2 bytes.Buffer
	v2.Write(pktLine("version 2\n"))
	v2.Write(pktLine("agent=git/2.47\n"))
	v2.Write(pktLine("fetch=shallow wait-for-done\n"))
	v2.WriteString("0000")

	adv, err := readAdvertisement(bufio.NewReader(&v2))
	if err != nil {
		t.Fatal(err)
	}
	if adv.Version != 2 {
		t.Fatalf("应识别为 v2，实际 %d", adv.Version)
	}
	if len(adv.Refs) != 0 {
		t.Fatalf("v2 广告里不该有 ref: %v", adv.Refs)
	}
	if !adv.Caps["fetch"] {
		t.Fatal("应记录 fetch 能力")
	}

	sha := strings.Repeat("a", 40)
	var v0 bytes.Buffer
	v0.WriteString("001e# service=git-upload-pack\n0000")
	v0.Write(pktLine(sha + " refs/heads/main\x00report-status side-band\n"))
	v0.WriteString("0000")
	adv, err = readAdvertisement(bufio.NewReader(&v0))
	if err != nil {
		t.Fatal(err)
	}
	if adv.Version != 0 || adv.Refs["refs/heads/main"] != sha {
		t.Fatalf("v0 解析错误: %+v", adv)
	}
}
