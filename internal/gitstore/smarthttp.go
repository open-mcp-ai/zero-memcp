package gitstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
)

func PktLine(s string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(s)+4, s))
}

func PktFlush() []byte { return []byte("0000") }

// PktLineBytes 是二进制安全的 pkt-line 编码。
//
// 为什么不能只用 PktLine(string)：side-band 数据帧的载荷是**原始 pack 字节**，
// 里面几乎必然出现非法 UTF-8 序列。Go 把 []byte 转 string 时对非法序列
// 会做替换（U+FFFD），长度随之变化——于是 "%04x" 里报的长度与实际字节数
// 不一致，客户端按长度切帧，切到的边界全是错的：表现为
// 「bad band #N」「unexpected EOF」这类完全指错方向的错误。
func PktLineBytes(p []byte) []byte {
	head := fmt.Sprintf("%04x", len(p)+4)
	out := make([]byte, 0, len(head)+len(p))
	out = append(out, head...)
	out = append(out, p...)
	return out
}

func ReadPktLine(r io.Reader) (string, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return "", err
	}
	var n int
	if _, err := fmt.Sscanf(string(head[:]), "%04x", &n); err != nil {
		return "", fmt.Errorf("invalid pkt-line length header %q", string(head[:]))
	}
	if n == 0 {
		return "", nil
	}
	if n < 4 {
		return "", fmt.Errorf("pkt-line length %d is less than 4", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(body), "\n"), nil
}

func (r *Repo) AdvertiseRefs(defaultBranch string) ([]byte, error) {
	return r.AdvertiseRefsFiltered(defaultBranch, nil)
}

// AdvertiseRefsFiltered 只广告 keep 放行的 ref。
//
// 传 nil 表示不筛选。筛选必须发生在广告阶段而不是 fetch 之后：
// 一旦 ref 出现在广告里，客户端就已经知道它存在、也知道它的 commit。
func (r *Repo) AdvertiseRefsFiltered(defaultBranch string, keep func(ref string) bool) ([]byte, error) {
	return r.advertise(defaultBranch, keep, "git-upload-pack",
		"symref=HEAD:refs/heads/%s side-band-64k ofs-delta agent=zero-memcp")
}

// AdvertiseReceivePack 广告可推送的 ref。
//
// 能力位刻意只有 report-status ofs-delta：
//   - 不广告 side-band-64k：客户端一旦协商成功，就会期待 report-status
//     被包进 0x01 通道里。我们回的是裸 pkt-line，它只会看到
//     「bad band #117」这种完全指不到病因的报错；
//   - 不广告 delete-refs（不允许删分支）、不广告 atomic（不做全成功或全失败）。
//
// 客户端看到什么就会要求什么，广告一个实现不了的能力等于埋一个必然的失败。
func (r *Repo) AdvertiseReceivePack(defaultBranch string, keep func(ref string) bool) ([]byte, error) {
	return r.advertise(defaultBranch, keep, "git-receive-pack",
		"report-status ofs-delta agent=zero-memcp")
}

func (r *Repo) advertise(defaultBranch string, keep func(ref string) bool, service, capsFormat string) ([]byte, error) {
	refs, err := r.Refs("refs/heads/")
	if err != nil {
		return nil, err
	}
	allow := func(ref string) bool { return keep == nil || keep(ref) }

	var buf bytes.Buffer
	buf.Write(PktLine("# service=" + service + "\n"))
	buf.Write(PktFlush())

	if defaultBranch == "" {
		defaultBranch = DefaultBranch
	}
	caps := capsFormat
	if strings.Contains(capsFormat, "%s") {
		caps = fmt.Sprintf(capsFormat, defaultBranch)
	}

	first := true
	emit := func(sha, name string) {
		if first {
			buf.Write(PktLine(fmt.Sprintf("%s %s\x00%s\n", sha, name, caps)))
			first = false
		} else {
			buf.Write(PktLine(fmt.Sprintf("%s %s\n", sha, name)))
		}
	}

	if headSHA, _ := r.Resolve(defaultBranch); headSHA != "" && allow("refs/heads/"+defaultBranch) {
		emit(headSHA, "HEAD")
	}

	for _, ref := range refs {
		name := strings.TrimPrefix(ref, "refs/heads/")
		if !allow("refs/heads/" + name) {
			continue
		}
		sha, err := r.Resolve(name)
		if err != nil || sha == "" {
			continue
		}
		emit(sha, "refs/heads/"+name)
	}

	if tags, err := r.Tags(); err == nil {
		names := make([]string, 0, len(tags))
		for n := range tags {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if !allow("refs/tags/" + n) {
				continue
			}
			emit(tags[n], "refs/tags/"+n)
		}
	}

	// 空仓库规范：没有任何 ref 可广播时，Git 要求服务端发一行全 0 的
	// capabilities^{} 伪 ref，后面带 capability 声明。
	//
	// 不发这一行（直接 flush 结束）的后果：客户端在 receive-pack 握手时
	// 拿不到服务端 capabilities，以为不支持 report-status。等服务端解包
	// 成功、回送 "unpack ok / ok refs/heads/main" 时，客户端把它当作协议
	// 异常，报「无法推送一些引用」——而服务端明明已经收下了。症状是
	// 新节点永远推不上第一条分支，且服务端日志干净。
	if first {
		buf.Write(PktLine(fmt.Sprintf("%s capabilities^{}\x00%s\n", zeroOID, caps)))
		buf.Write(PktFlush())
		return buf.Bytes(), nil
	}

	buf.Write(PktFlush())
	return buf.Bytes(), nil
}

func (r *Repo) UploadPackResponse(body io.Reader) ([]byte, error) {
	return r.UploadPackResponseFiltered(body, nil)
}

// uploadPackRequest 是解析后的 upload-pack 请求。
//
// 单独拆出来是为了让「解析失败」和「写响应」分开：解析失败还能回 500，
// 一旦开始写字节就只能截断。
type uploadPackRequest struct {
	wants    map[string]bool
	haves    map[string]bool
	sideband bool
	// done 表示客户端已经结束协商（发了 done）。
	//
	// 这个位不是可有可无的元数据：git 的 v0 协商是多轮的。
	// 客户端先发 want + 一批 have + flush（**没有 done**），服务端这一轮
	// 只允许回 ACK/NAK，不能发 pack；客户端据此再发下一批 have，
	// 直到它自己认为够了才发 done。
	//
	// 踩过的坑（2026-11-13）：解析器读到 done 就 break，却没记下这件事，
	// 于是每一轮都当最终轮处理——客户端首轮就收到 pack，第二轮客户端
	// 的解析器在期待 ACK/NAK 的位置读到 "PACK"，报
	// 「expected ACK/NAK, got '?'」——一个完全指错方向的错误信息。
	// 顺带每次协商都白算一个完整包（现场实测 14 秒）。
	done bool
}

// ParseUploadPackRequest 读出 want/have 与 sideband 能力。
//
// have 必须读。早先这里只认 want，have 全丢——服务端于是永远不知道
// 客户端已经有什么，每次都把整段历史重发一遍。症状不是报错，是
// 「同步越来越慢」：1.1 万篇文档的仓库一次全量 30MB+，五分钟一轮，
// 客户端 90s 超时，同步静默失败。增量协议的意义就在 have 这一行。
func (r *Repo) ParseUploadPackRequest(body io.Reader, allowSHA func(string) bool) (*uploadPackRequest, error) {
	req := &uploadPackRequest{wants: map[string]bool{}, haves: map[string]bool{}}
	for {
		line, err := ReadPktLine(body)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		// 空行是**分段**，不是结束。
		//
		// git 的协商是两段：want... 然后 flush，再 have... 最后 done。
		// 早先这里遇到空行就 break，于是所有 have 都被丢掉——服务端
		// 永远以为客户端什么都没有，每轮都发全量。这正是「同步越来
		// 越慢、最后超时」的根因。
		if line == "" {
			continue
		}
		if line == "done" {
			req.done = true
			break
		}
		if strings.Contains(line, "side-band-64k") || strings.Contains(line, "side-band") {
			req.sideband = true
		}
		fields := strings.Fields(strings.SplitN(line, "\x00", 2)[0])
		if len(fields) < 2 || validSHA(fields[1]) != nil {
			continue
		}
		switch fields[0] {
		case "want":
			if allowSHA != nil && !allowSHA(fields[1]) {
				return nil, fmt.Errorf("want %s is not an advertised ref this credential may read", fields[1])
			}
			req.wants[fields[1]] = true
		case "have":
			// have 不做 allow 检查：它只是「我这边有这些」，不产生数据外泄。
			// 未知对象在算差集时被忽略（见 WriteUploadPack）。
			req.haves[fields[1]] = true
		}
	}
	if len(req.wants) == 0 {
		return nil, fmt.Errorf("client requested no wants")
	}
	return req, nil
}

// UploadPackResponseFiltered 只允许客户端请求 allowSHA 放行的 advertised tip。
// 不检查 want 会让调用方手工猜 SHA 绕过 info/refs 的分支过滤。
//
// 保留这个一次性的版本给调用方与测试用；HTTP 路径走流式，见 WriteUploadPack。
func (r *Repo) UploadPackResponseFiltered(body io.Reader, allowSHA func(string) bool) ([]byte, error) {
	req, err := r.ParseUploadPackRequest(body, allowSHA)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := r.WriteUploadPack(&out, req); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// packChunkWriter 把字节流切成 side-band 分包，或原样透传。
type packChunkWriter struct {
	out      io.Writer
	sideband bool
	buf      []byte
}

const sideBandMaxChunk = 65515

func (w *packChunkWriter) Write(p []byte) (int, error) {
	if !w.sideband {
		return w.out.Write(p)
	}
	w.buf = append(w.buf, p...)
	for len(w.buf) >= sideBandMaxChunk {
		chunk := make([]byte, sideBandMaxChunk+1)
		chunk[0] = 0x01
		copy(chunk[1:], w.buf[:sideBandMaxChunk])
		if _, err := w.out.Write(PktLineBytes(chunk)); err != nil {
			return 0, err
		}
		w.buf = w.buf[sideBandMaxChunk:]
	}
	return len(p), nil
}

func (w *packChunkWriter) Close() error {
	if !w.sideband {
		return nil
	}
	if len(w.buf) > 0 {
		chunk := make([]byte, len(w.buf)+1)
		chunk[0] = 0x01
		copy(chunk[1:], w.buf)
		if _, err := w.out.Write(PktLineBytes(chunk)); err != nil {
			return err
		}
		w.buf = nil
	}
	_, err := w.out.Write(PktFlush())
	return err
}

// hashTee 让每个字节既进 sha1 又进下游——pack 尾部的校验和必须覆盖
// 原始 pack 字节，而不是 side-band 分包的字节。
type hashTee struct {
	h   hash.Hash
	out io.Writer
}

func (t *hashTee) Write(p []byte) (int, error) {
	t.h.Write(p)
	return t.out.Write(p)
}

// WriteUploadPack 流式产出 upload-pack 响应。
//
// 为什么不是先拼好再返回：整包在内存里攒完才写，客户端在生成期间
// 一个字节都收不到。仓库一大，客户端先超时，服务端还在算——双方
// 都以为对方死了。流式之后首字节立刻到，内存占用从 O(整包) 降到
// O(单个对象)。
func (r *Repo) WriteUploadPack(out io.Writer, req *uploadPackRequest) error {
	// 共同基点：客户端声明「我有」而我们确实持有的第一个对象。
	//
	// 没有这一步，客户端会一路 NAK 到协商超时，最后判定「没有共同的提交」，
	// 然后发一个**不带 have 的** done 请求——差集就永远用不上，
	// 每轮同步都是全量。现场实测：15,533 个对象、124MB、71 秒。
	common := ""
	for sha := range req.haves {
		if r.HasObject(sha) {
			common = sha
			break
		}
	}

	if !req.done {
		// 协商中间轮次：只回 ACK/NAK，不发包。
		//
		// 这一行的代价是「多发一个空响应」，收益是客户端不会在自己还没说完
		// have 的时候收到一个包（省掉它会白算一遍完整 pack 然后报协议错）。
		if common != "" {
			_, err := out.Write(PktLine("ACK " + common + "\n"))
			return err
		}
		_, err := out.Write(PktLine("NAK\n"))
		return err
	}

	// 最终轮：ACK/NAK 一律发，然后再发包。
	//
	// 这一行是拿真实 git 当参照物量出来的，不是猜的：
	//   git upload-pack --stateless-rpc 对「want + done」这种克隆形态请求
	//   的响应头正是 "0008NAK\n" 后紧跟 sideband 帧（0021 02 ...）。
	// 也就是说 ACK/NAK 是**裸 pkt-line**，即使协商了 side-band-64k 也不进带外通道。
	//
	// 踩过的坑（2026-11-13，bad band #78 / #65）：症状看起来像「多余的那行
	// NAK 被当成 sideband 帧」，于是先后试过「不发 NAK」「只发 ACK」，
	// 结果把全新克隆搞坏（expected ACK/NAK, got '?PACK'）。
	// 真正的原因是 sideband 分帧用 PktLine(string(...)) 把二进制 pack 字节
	// 过了 UTF-8：非法序列被替换、长度头失真，客户端按长度切帧全错，
	// 于是它读到那一行「看起来正常」的 NAK 时就把它当成了第一帧。
	// 修在 PktLineBytes 上之后，NAK 本来就该照发。
	if common != "" {
		if _, err := out.Write(PktLine("ACK " + common + "\n")); err != nil {
			return err
		}
	} else {
		if _, err := out.Write(PktLine("NAK\n")); err != nil {
			return err
		}
	}

	// 短路：客户端要的每一个 tip 它自己都已经有了。
	//
	// 为什么要单独判（2026-11-24 实测）：两端已经对齐时，客户端仍然会发
	// want（它不知道对端走到了哪里）+ have。这时全量闭包算完再减去 have 闭包，
	// 结果必然是空——但这个「必然」要花掉 10~13 秒真实磁盘读（3060 冷页缓存下
	// 直接撞 90 秒超时）。而「want 的 sha 就在 haves 里」这一个判断就等价于
	// 「可达集已被 have 覆盖」：want 是一个 tip，have 是同一个 sha，两者闭包
	// 完全相同，差集为空。
	//
	// 这一判必须在遍历**之前**：放在后面就只能省下 have 闭包那一轮，
	// 而 want 闭包本身就是那次 90 秒。
	//
	// 不发空 pack 而直接 return：pack 头里写 0 个对象是合法但没必要的往返，
	// 而 ACK 已经在上面发出去了，客户端会把它读成「什么都没有，收工」。
	if len(req.wants) > 0 && len(req.haves) > 0 {
		covered := true
		for sha := range req.wants {
			if !req.haves[sha] {
				covered = false
				break
			}
		}
		if covered {
			return nil
		}
	}

	objects := map[string]bool{}
	for sha := range req.wants {
		if err := r.collectReachable(sha, objects); err != nil {
			return fmt.Errorf("collect objects for %s: %w", sha, err)
		}
	}

	// 差集：客户端已经有的对象不再发送。
	//
	// have 指向的对象在本仓库不存在时（对端历史、已 gc 掉的）忽略即可——
	// 差集少减一点只会多发，不会发错。
	//
	// continuation 场景（最终轮不带 have）要把上一轮记住的共同基点也算进来，
	// 否则「协商成功」只体现在 ACK 上，包里还是全量。
	if len(req.haves) > 0 {
		already := map[string]bool{}
		for sha := range req.haves {
			if err := r.collectReachable(sha, already); err != nil {
				continue
			}
		}
		for sha := range already {
			delete(objects, sha)
		}
	}

	chunks := &packChunkWriter{out: out, sideband: req.sideband}
	tee := &hashTee{h: sha1.New(), out: chunks}

	var head bytes.Buffer
	head.WriteString("PACK")
	_ = binary.Write(&head, binary.BigEndian, uint32(2))
	_ = binary.Write(&head, binary.BigEndian, uint32(len(objects)))
	if _, err := tee.Write(head.Bytes()); err != nil {
		return err
	}

	// 顺序按 类型→大小→sha 固定下来：同一份内容每次产出的 pack 一致，
	// 便于比对与测试。
	shas := make([]string, 0, len(objects))
	for s := range objects {
		shas = append(shas, s)
	}
	sort.Strings(shas)

	for _, sha := range shas {
		typ, body, err := r.readObject(sha)
		if err != nil {
			return err
		}
		hdr := encodePackHeader(typeCode(typ), int64(len(body)))
		if _, err := tee.Write(hdr); err != nil {
			return err
		}
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		if _, err := zw.Write(body); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		if _, err := tee.Write(zbuf.Bytes()); err != nil {
			return err
		}
	}

	if _, err := tee.Write(tee.h.Sum(nil)); err != nil {
		return err
	}
	return chunks.Close()
}

func (r *Repo) collectReachable(sha string, out map[string]bool) error {
	if out[sha] {
		return nil
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return err
	}
	out[sha] = true

	switch typ {
	case "commit":
		c := parseCommit(body)
		if c.Tree != "" {
			if err := r.collectReachable(c.Tree, out); err != nil {
				return err
			}
		}
		for _, p := range c.Parents {
			if err := r.collectReachable(p, out); err != nil {
				return err
			}
		}
	case "tree":
		items, err := parseTree(body)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := r.collectReachable(it.sha, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Repo) buildPack(objects map[string]bool) ([]byte, error) {
	shas := make([]string, 0, len(objects))
	for s := range objects {
		shas = append(shas, s)
	}
	type item struct {
		sha  string
		typ  string
		size int
		body []byte
	}
	items := make([]item, 0, len(shas))
	for _, sha := range shas {
		typ, body, err := r.readObject(sha)
		if err != nil {
			return nil, err
		}
		items = append(items, item{sha: sha, typ: typ, size: len(body), body: body})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].typ != items[j].typ {
			return items[i].typ < items[j].typ
		}
		if items[i].size != items[j].size {
			return items[i].size < items[j].size
		}
		return items[i].sha < items[j].sha
	})

	var buf bytes.Buffer
	h := sha1.New()
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(items)))
	h.Write(buf.Bytes())

	for _, it := range items {
		hdr := encodePackHeader(typeCode(it.typ), int64(len(it.body)))
		buf.Write(hdr)
		h.Write(hdr)

		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		if _, err := zw.Write(it.body); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		buf.Write(zbuf.Bytes())
		h.Write(zbuf.Bytes())
	}

	buf.Write(h.Sum(nil))
	return buf.Bytes(), nil
}

func typeCode(t string) int {
	switch t {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 3
}

func encodePackHeader(typ int, size int64) []byte {
	c := byte((typ & 0x7) << 4)
	c |= byte(size & 0x0f)
	size >>= 4
	out := []byte{c}
	for size > 0 {
		out[len(out)-1] |= 0x80
		out = append(out, byte(size&0x7f))
		size >>= 7
	}
	return out
}

// BuildPack 为推送生成一个 pack：包含 tips 可达的全部对象，
// 并排除 exclude 可达的部分（远端已有的历史）。
//
// exclude 里的对象是「远端已有」的近似：如果远端和本地有分叉，
// 多排除会漏对象、少排除只是多传。所以只在本地确实能解析该 commit 时
// 才用它做排除基准；解析不了就传全量——宁可慢，也不能推一个残缺的 pack。
func (r *Repo) BuildPack(tips, exclude []string) ([]byte, error) {
	want := map[string]bool{}
	for _, tip := range tips {
		if err := r.collectReachable(tip, want); err != nil {
			return nil, fmt.Errorf("collect objects for %s: %w", tip, err)
		}
	}
	for _, tip := range exclude {
		if tip == "" || strings.Trim(tip, "0") == "" {
			continue
		}
		if _, _, err := r.readObject(tip); err != nil {
			continue
		}
		have := map[string]bool{}
		if err := r.collectReachable(tip, have); err != nil {
			continue
		}
		for sha := range have {
			delete(want, sha)
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	return r.buildPack(want)
}
