package gitstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
)

func PktLine(s string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(s)+4, s))
}

func PktFlush() []byte { return []byte("0000") }

func ReadPktLine(r io.Reader) (string, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return "", err
	}
	var n int
	if _, err := fmt.Sscanf(string(head[:]), "%04x", &n); err != nil {
		return "", fmt.Errorf("非法 pkt-line 长度头 %q", string(head[:]))
	}
	if n == 0 {
		return "", nil
	}
	if n < 4 {
		return "", fmt.Errorf("pkt-line 长度 %d 小于 4", n)
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
		defaultBranch = "zero"
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

	if first {
		buf.Write(PktFlush())
		return buf.Bytes(), nil
	}

	buf.Write(PktFlush())
	return buf.Bytes(), nil
}

func (r *Repo) UploadPackResponse(body io.Reader) ([]byte, error) {
	return r.UploadPackResponseFiltered(body, nil)
}

// UploadPackResponseFiltered 只允许客户端请求 allowSHA 放行的 advertised tip。
// 不检查 want 会让调用方手工猜 SHA 绕过 info/refs 的分支过滤。
func (r *Repo) UploadPackResponseFiltered(body io.Reader, allowSHA func(string) bool) ([]byte, error) {
	wants := map[string]bool{}
	// 客户端声明的能力位。side-band 只有请求了才用：
	// 不请求也照样分包，客户端会把它当成原始 pack 直接读——
	// 症状是「不是 pack 数据（缺 PACK 头）」，而 pack 就在分包里。
	sideband := false
	for {
		line, err := ReadPktLine(body)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if line == "" {
			break
		}
		if strings.Contains(line, "side-band-64k") || strings.Contains(line, "side-band") {
			sideband = true
		}
		if strings.HasPrefix(line, "want ") {
			fields := strings.Fields(strings.SplitN(line, "\x00", 2)[0])
			if len(fields) >= 2 && validSHA(fields[1]) == nil {
				if allowSHA != nil && !allowSHA(fields[1]) {
					return nil, fmt.Errorf("want %s 不是该凭证可读取的 advertised ref", fields[1])
				}
				wants[fields[1]] = true
			}
		}
	}
	if len(wants) == 0 {
		return nil, fmt.Errorf("客户端没有请求任何 want")
	}

	objects := map[string]bool{}
	for sha := range wants {
		if err := r.collectReachable(sha, objects); err != nil {
			return nil, fmt.Errorf("收集对象 %s: %w", sha, err)
		}
	}

	pack, err := r.buildPack(objects)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Write(PktLine("NAK" + "\n"))
	if !sideband {
		out.Write(pack)
		out.Write(PktFlush())
		return out.Bytes(), nil
	}

	const maxChunk = 65515
	for len(pack) > 0 {
		n := maxChunk
		if len(pack) < n {
			n = len(pack)
		}
		out.Write(PktLine(string(append([]byte{0x01}, pack[:n]...))))
		pack = pack[n:]
	}
	out.Write(PktFlush())
	return out.Bytes(), nil
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
			return nil, fmt.Errorf("收集对象 %s: %w", tip, err)
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
