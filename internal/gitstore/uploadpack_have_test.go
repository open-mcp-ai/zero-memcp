package gitstore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// 锁住两件事：
//  1. have 能被解析且真正生效（差集不发送客户端已有对象）；
//  2. 流式直写产出合法 pack，包含 NAK 头与可解析的 PACK 二进制。
func TestUploadPackHaveDiffAndStreaming(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	branch := "main"

	// 提交 c1
	if err := r.Write(branch, "f1.md", []byte("base content"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c1, err := r.Commit(branch, "c1")
	if err != nil {
		t.Fatal(err)
	}

	// 提交 c2（在 c1 之上）
	if err := r.Write(branch, "f2.md", []byte("second change"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c2, err := r.Commit(branch, "c2")
	if err != nil {
		t.Fatal(err)
	}

	// 场景 A：无 have（全量取 c2）
	bodyFull := bytes.NewBuffer(append(
		append(PktLine("want "+c2+"\n"), []byte("0000")...),
		[]byte("0009done\n")...,
	))
	respFull, err := r.UploadPackResponse(bodyFull)
	if err != nil {
		t.Fatalf("full pack failed: %v", err)
	}
	// 不写 NAK：客户端发 done 之后直接进入 side-band 解复用，
	// 任何一行明文 pkt-line 都会被当成 sideband 帧（bad band #78）。
	if !strings.HasPrefix(string(respFull), "0008NAK\nPACK") {
		t.Fatalf("pack 必须直接从 PACK 开始（不发 NAK），实际前 20 字节: %q", respFull[:20])
	}

	// 场景 B：客户端声明 have c1（增量取 c2）
	bodyIncr := bytes.NewBuffer(append(
		append(
			append(PktLine("want "+c2+"\n"), []byte("0000")...),
			PktLine("have "+c1+"\n")...,
		),
		[]byte("0009done\n")...,
	))
	respIncr, err := r.UploadPackResponse(bodyIncr)
	if err != nil {
		t.Fatalf("incremental pack failed: %v", err)
	}

	// 增量必须显著小于全量：c1 相关对象（blob + 旧 tree + c1 commit）被剪掉
	if len(respIncr) >= len(respFull) {
		t.Fatalf("have 未生效：增量包大小 %d 应当小于全量包 %d", len(respIncr), len(respFull))
	}

	// 场景 C：流式直写产生的字节与一次性版本必须一致
	bodyStream := bytes.NewBuffer(append(
		append(PktLine("want "+c2+"\n"), []byte("0000")...),
		[]byte("0009done\n")...,
	))
	req, err := r.ParseUploadPackRequest(bodyStream, nil)
	if err != nil {
		t.Fatal(err)
	}
	var streamed bytes.Buffer
	if err := r.WriteUploadPack(&streamed, req); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamed.Bytes(), respFull) {
		t.Fatalf("流式输出与一次性版本不一致：streamed=%d full=%d", streamed.Len(), len(respFull))
	}

	// 场景 D：未知 have 优雅忽略，不报错、不崩溃
	bodyUnknown := bytes.NewBuffer(append(
		append(
			append(PktLine("want "+c2+"\n"), []byte("0000")...),
			PktLine(fmt.Sprintf("have %040x\n", 0xdeadbeef))...,
		),
		[]byte("0009done\n")...,
	))
	if _, err := r.UploadPackResponse(bodyUnknown); err != nil {
		t.Fatalf("未知的 have 应该被静默忽略，实际报错: %v", err)
	}
}

// 真实现场（2026-11-13，Mac ↔ 3060 同步）：
//
//	客户端首轮发 want + 一批 have + flush，**没有 done**（多轮协商）。
//	服务端每一轮都当最终轮处理，直接回 NAK + 完整 pack（现场白算 14 秒）。
//	客户端第二轮发送时在期待 ACK/NAK 的位置读到 PACK，报
//	「expected ACK/NAK, got '?PACK'」——一个指向协议层之外的错误。
//
// 钉住：没有 done 的请求只允许回 NAK，一个 pack 字节都不能有。
func TestUploadPackNegotiationRoundReturnsOnlyNAK(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("main", "c1"); err != nil {
		t.Fatal(err)
	}
	tip, err := r.Resolve("main")
	if err != nil {
		t.Fatal(err)
	}

	// 只要 want，没有任何 have，也没有 done —— 这是协商的第一轮。
	var req bytes.Buffer
	req.Write(PktLine("want " + tip + " side-band-64k ofs-delta\n"))
	req.Write(PktFlush())

	var out bytes.Buffer
	if err := r.WriteUploadPack(&out, mustParseUploadPack(t, r, req.Bytes())); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "NAK") {
		t.Fatalf("negotiation round must answer NAK, got %q", got)
	}
	if strings.Contains(got, "PACK") {
		t.Fatalf("negotiation round must not send a pack, got %q", got)
	}

	// 同一份 want 加上 done：这一轮才发包。
	var final bytes.Buffer
	final.Write(PktLine("want " + tip + " side-band-64k ofs-delta\n"))
	final.Write(PktFlush())
	final.Write(PktLine("done\n"))

	var out2 bytes.Buffer
	if err := r.WriteUploadPack(&out2, mustParseUploadPack(t, r, final.Bytes())); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "PACK") {
		t.Fatalf("final round (done) must send a pack, got %q", out2.String())
	}
}

// 带 done 的 have 差集请求仍然只发缺的对象。
//
// 判据用「pack 头里的对象计数」而不是「body 里有没有那个 SHA 字样」：
// 对象在 pack 里是 zlib 压缩的，SHA 不会以明文出现；而 ACK 行里**会**
// 出现 SHA——用它当判据会把一个正确的实现判成泄漏。
func TestUploadPackDoneWithHavesSendsOnlyDifference(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	old, err := r.Commit("main", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "b.md", []byte("b"), ModeContent); err != nil {
		t.Fatal(err)
	}
	tip, err := r.Commit("main", "c2")
	if err != nil {
		t.Fatal(err)
	}

	// 全量：不带 have。
	var fullReq bytes.Buffer
	fullReq.Write(PktLine("want " + tip + " ofs-delta\n"))
	fullReq.Write(PktFlush())
	fullReq.Write(PktLine("done\n"))
	var full bytes.Buffer
	if err := r.WriteUploadPack(&full, mustParseUploadPack(t, r, fullReq.Bytes())); err != nil {
		t.Fatal(err)
	}

	// 增量：声明已有 c1。
	var incReq bytes.Buffer
	incReq.Write(PktLine("want " + tip + " ofs-delta\n"))
	incReq.Write(PktFlush())
	incReq.Write(PktLine("have " + old + "\n"))
	incReq.Write(PktLine("done\n"))
	var inc bytes.Buffer
	if err := r.WriteUploadPack(&inc, mustParseUploadPack(t, r, incReq.Bytes())); err != nil {
		t.Fatal(err)
	}

	fullN := packObjectCount(t, full.Bytes())
	incN := packObjectCount(t, inc.Bytes())
	if incN >= fullN {
		t.Errorf("have 没有生效：全量 %d 个对象，带 have 的仍然 %d 个", fullN, incN)
	}
	if incN == 0 {
		t.Error("增量包是空的，场景构造有误")
	}
}

// packObjectCount 跳过前导 pkt-line（ACK/NAK），读出 PACK 头里的对象计数。
func packObjectCount(t *testing.T, b []byte) int {
	t.Helper()
	i := bytes.Index(b, []byte("PACK"))
	if i < 0 {
		t.Fatalf("no PACK header in %q", string(b[:min(len(b), 40)]))
	}
	if len(b) < i+12 {
		t.Fatalf("PACK header truncated")
	}
	return int(binary.BigEndian.Uint32(b[i+8 : i+12]))
}

// mustParseUploadPack 解析一段 upload-pack 请求体，失败即终止测试。
func mustParseUploadPack(t *testing.T, r *Repo, body []byte) *uploadPackRequest {
	t.Helper()
	req, err := r.ParseUploadPackRequest(bytes.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parse upload-pack request: %v", err)
	}
	return req
}

// 真实现场（2026-11-13，第二次 Mac ↔ 3060 fetch）：
//
//	握手已经 ACK 过共同提交，客户端随后发 done；服务端在最终轮又回了一个
//	裸的 "NAK\n"。客户端此时已经在 side-band-64k 模式，把这一行当 sideband
//	帧解析，取首字节 'N'(0x4e=78) 当 band 编号，报
//	「protocol error: bad band #78」。
//
// 钉住：有共同提交时，最终轮不许出现裸 NAK；pack 必须直接跟在后面。
func TestUploadPackFinalRoundDoesNotEmitBareNAKWhenCommonExists(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	old, err := r.Commit("main", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "b.md", []byte("b"), ModeContent); err != nil {
		t.Fatal(err)
	}
	tip, err := r.Commit("main", "c2")
	if err != nil {
		t.Fatal(err)
	}

	var req bytes.Buffer
	req.Write(PktLine("want " + tip + " side-band-64k ofs-delta\n"))
	req.Write(PktFlush())
	req.Write(PktLine("have " + old + "\n"))
	req.Write(PktLine("done\n"))

	var out bytes.Buffer
	if err := r.WriteUploadPack(&out, mustParseUploadPack(t, r, req.Bytes())); err != nil {
		t.Fatal(err)
	}
	body := out.Bytes()
	if _, err := r.Resolve("main"); err != nil {
		t.Fatal(err)
	}
	packAt := bytes.Index(body, []byte("PACK"))
	if packAt < 0 {
		t.Fatalf("no pack written: %q", string(body[:min(len(body), 60)]))
	}
	if bytes.Contains(body[:packAt], []byte("NAK")) {
		t.Errorf("final round emitted a bare NAK before the pack (client reads it as sideband): %q", string(body[:packAt]))
	}
}

// 真实现场（2026-11-13，第三轮）：两轮协商的 fetch。
//
// 客户端第一轮拿到 ACK，最终轮只发 "want ... done"（不带 have）。
// 最终轮仍然必须发 ACK/NAK —— 这是拿真实 git 量出来的契约
// （git upload-pack --stateless-rpc 的响应头就是 "0008NAK\n" 后跟 sideband 帧）。
// 当时的 bad band #78 不是这行 NAK 造成的，而是 sideband 分帧把二进制 pack
// 过了 UTF-8（见 TestPktLineBytesIsBinarySafe）。
func TestUploadPackFinalRoundAlwaysEmitsACKOrNAK(t *testing.T) {
	r := newTestRepo(t)
	if err := r.Write("main", "a.md", []byte("a"), ModeContent); err != nil {
		t.Fatal(err)
	}
	old, err := r.Commit("main", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("main", "b.md", []byte("b"), ModeContent); err != nil {
		t.Fatal(err)
	}
	tip, err := r.Commit("main", "c2")
	if err != nil {
		t.Fatal(err)
	}

	// 第一轮：want + have（无 done）→ ACK。
	var r1 bytes.Buffer
	r1.Write(PktLine("want " + tip + " side-band-64k ofs-delta\n"))
	r1.Write(PktFlush())
	r1.Write(PktLine("have " + old + "\n"))
	var o1 bytes.Buffer
	if err := r.WriteUploadPack(&o1, mustParseUploadPack(t, r, r1.Bytes())); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o1.String(), "ACK "+old) {
		t.Fatalf("first round should ACK the common have, got %q", o1.String())
	}

	// 最终轮：want + done（无 have）。没有 have 就没法算差集，
	// 但 ACK/NAK 必须发——真实客户端在等它。
	var r2 bytes.Buffer
	r2.Write(PktLine("want " + tip + " side-band-64k ofs-delta\n"))
	r2.Write(PktFlush())
	r2.Write(PktLine("done\n"))
	var o2 bytes.Buffer
	if err := r.WriteUploadPack(&o2, mustParseUploadPack(t, r, r2.Bytes())); err != nil {
		t.Fatal(err)
	}
	body := o2.String()
	if !strings.HasPrefix(body, "0008NAK\n") && !strings.HasPrefix(body, "00") {
		t.Errorf("final round must start with a pkt-line ACK/NAK, got %q", body[:min(len(body), 40)])
	}
	if !strings.Contains(body, "NAK") && !strings.Contains(body, "ACK") {
		t.Errorf("final round emitted neither ACK nor NAK: %q", body[:min(len(body), 40)])
	}
}


// 短路：当 want 的每个 tip 都在 have 里时，服务端绝不去读对象。
//
// 为什么需要这一条（2026-11-24 实测）：已对齐时，两端历史完全一样，
// want 闭包减去 have 闭包结果必然为空。但全量遍历要读几万个松散/打包对象，
// 冷页缓存下直接耗尽 90 秒预算。短路使对齐时的响应从 10~13 秒降为 0 秒。
//
// 证明「绝不读对象」的手段：构造一个 tip 指向合法 SHA，但在库里根本没有对应
// 对象；同时声明该 tip 也在 have 里。未短路的实现会立刻报
// "open object ...: no such file"，只有短路成功的实现才会静默通过并回 ACK。
func TestUploadPackShortCircuitsWhenWantsAlreadyInHaves(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 真实提交 c1（在库里真实存在，作为共同基点被 ACK）
	if err := r.Write("main", "f.md", []byte("base"), ModeContent); err != nil {
		t.Fatal(err)
	}
	c1, err := r.Commit("main", "c1")
	if err != nil {
		t.Fatal(err)
	}

	// 伪造一个库里根本不存在的 SHA：
	phantom := "1111111111111111111111111111111111111111"

	// 场景 A：want 包含 phantom，且 phantom 也在 have 里 → 必须短路，不读对象
	var bodyShort bytes.Buffer
	bodyShort.Write(PktLine("want " + c1 + "\n"))
	bodyShort.Write(PktLine("want " + phantom + "\n"))
	bodyShort.Write(PktFlush())
	bodyShort.Write(PktLine("have " + c1 + "\n"))
	bodyShort.Write(PktLine("have " + phantom + "\n"))
	bodyShort.Write(PktLine("done\n"))

	resp, err := r.UploadPackResponse(&bodyShort)
	if err != nil {
		t.Fatalf("want 全部在 have 里时应短路通过，实际失败: %v", err)
	}
	if !strings.Contains(string(resp), "ACK") {
		t.Fatalf("应回 ACK，实际: %q", string(resp))
	}
	if strings.Contains(string(resp), "PACK") {
		t.Fatalf("短路时不应生成 PACK，实际包含: %q", string(resp))
	}

	// 场景 B（反向对照）：同一个 phantom 不在 have 里时，必须真正去读它并失败。
	// 没有这一条，上面那个断言在「永远不读对象」的假实现下也会通过。
	var bodyFail bytes.Buffer
	bodyFail.Write(PktLine("want " + phantom + "\n"))
	bodyFail.Write(PktFlush())
	bodyFail.Write(PktLine("have " + c1 + "\n"))
	bodyFail.Write(PktLine("done\n"))
	if _, err := r.UploadPackResponse(&bodyFail); err == nil {
		t.Fatal("phantom 不在 have 里时必须因为读不到对象而报错")
	}
}
