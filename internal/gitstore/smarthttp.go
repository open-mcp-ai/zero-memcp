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
	refs, err := r.Refs("refs/heads/")
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.Write(PktLine("# service=git-upload-pack" + "\n"))
	buf.Write(PktFlush())

	if defaultBranch == "" {
		defaultBranch = "zero"
	}
	caps := fmt.Sprintf("symref=HEAD:refs/heads/%s side-band-64k ofs-delta agent=zero-memcp", defaultBranch)

	first := true
	emit := func(sha, name string) {
		if first {
			buf.Write(PktLine(fmt.Sprintf("%s %s\x00%s\n", sha, name, caps)))
			first = false
		} else {
			buf.Write(PktLine(fmt.Sprintf("%s %s\n", sha, name)))
		}
	}

	if headSHA, _ := r.Resolve(defaultBranch); headSHA != "" {
		emit(headSHA, "HEAD")
	}

	for _, ref := range refs {
		name := strings.TrimPrefix(ref, "refs/heads/")
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
	wants := map[string]bool{}
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
		if strings.HasPrefix(line, "want ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && validSHA(fields[1]) == nil {
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
