package gittransport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// FetchResult 报告一次取回的结果。
type FetchResult struct {
	// Branches 是远端这次广告的分支 → sha。
	Branches map[string]string
	// Objects/Written 来自 pack 接收统计。
	Objects int
	Written int
}

// 取回的边界：只更新 refs/remotes/<remote>/*，从不改本地分支。
// 「知道远端有什么」和「把远端变成我的」是两件事，混在一起就再也
// 分不清一次误操作到底改了本地还是只改了认知。

// branchScope 把「配置里声明取回哪些分支」编译成一次查询。
//
// wanted 是 [[remote]] 声明的分支白名单（工作层对端的 pull，或回落到 Git
// 远端的 push 列表）。**空表示一个都不取**：取回哪些分支必须是显式配置的，
// 不能由「对端广告了什么」决定。
//
// 为什么这条边界必须画在这里而不是取回之后再裁（2026-11-24 3060 链路卡死
// 的真因）：对象层面无法按分支隔离，want 一发出去，对端就会把整条历史的
// pack 打好发过来。meta.index 那种分支每次提交都是整份索引快照（本机 98
// 个 blob / 246 MB），混进 want 会让一次 fetch 在 90 秒里打不完包，表现为
// 「读响应超时」——看起来像网络问题，实际是自己在拉一份根本不该同步的缓存。
//
// "*" 是唯一的通配，用来显式表达「这个远端就是要全取」。
//
// 组装 want（fetchTips）与落地引用（applyFetch）必须共用这一个判据：
// 两份名单一旦漂移，要么多拉一整个分支，要么留下一个「没要过却存在」的
// 跟踪引用，下一轮 merge 拿它当真值。
func newBranchScope(wanted []string) branchScope {
	s := branchScope{match: make(map[string]bool, len(wanted))}
	for _, w := range wanted {
		if w == "*" {
			s.all = true
			return s
		}
		s.match[w] = true
	}
	return s
}

type branchScope struct {
	all   bool
	match map[string]bool
}

func (s branchScope) empty() bool { return !s.all && len(s.match) == 0 }

func (s branchScope) allows(branch string) bool {
	if s.all {
		return true
	}
	return s.match[branch]
}

// fetchTips 从广告里挑出要取的分支。scope 为空时一条都不取。
func fetchTips(refs map[string]string, wanted []string) []string {
	scope := newBranchScope(wanted)
	if scope.empty() {
		return nil
	}
	tips := make([]string, 0, len(refs))
	for ref, sha := range refs {
		if !strings.HasPrefix(ref, "refs/heads/") || !validSHA(sha) {
			continue
		}
		if !scope.allows(strings.TrimPrefix(ref, "refs/heads/")) {
			continue
		}
		tips = append(tips, sha)
	}
	return tips
}

// fetchBody 组装 v0 upload-pack 请求：
//
//	want <tip>...   flush   have <local tip>...   done
//
// 一次发完所有 have 而不是分轮协商：v0 的多轮协商要读写交替，
// 而 HTTP 是无状态的，每轮都得重发全部内容。一次性发完，
// 两种传输走同一份字节，也少一个「谈了几轮才谈拢」的状态机。
func fetchBody(tips, haves []string) []byte {
	var b []byte
	for i, sha := range tips {
		if i == 0 {
			// 第一条 want 带能力位。要 side-band-64k：真实 git 服务端
			// 几乎都会分包，主动声明才能名正言顺地解复用。
			b = append(b, pktLine(fmt.Sprintf("want %s multi_ack_detailed side-band-64k ofs-delta agent=zero-memcp\n", sha))...)
			continue
		}
		b = append(b, pktLine(fmt.Sprintf("want %s\n", sha))...)
	}
	b = append(b, flushPkt...)
	for _, sha := range haves {
		b = append(b, pktLine(fmt.Sprintf("have %s\n", sha))...)
	}
	b = append(b, pktLine("done\n")...)
	return b
}

// readFetchPack 读出协商结论与后面的 pack。
//
// 响应可能先来一串 "ACK <sha> common" / "ACK <sha> ready"（多轮协商的产物），
// 真正的结论是最后一条 "NAK" 或裸的 "ACK <sha>"。
// 只认第一条 ACK 会把后面的 ack 行当成 pack 头，然后报「不是 pack 数据」。
func readFetchPack(r *bufio.Reader) ([]byte, error) {
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			return nil, fmt.Errorf("read negotiation response: %w", err)
		}
		if kind == pktFlush || kind == pktDelim || kind == pktResponseEnd {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "ERR ") {
			return nil, fmt.Errorf("remote rejected fetch: %s", strings.TrimPrefix(trimmed, "ERR "))
		}
		fields := strings.Fields(trimmed)
		if fields[0] == "NAK" {
			break
		}
		if fields[0] == "ACK" && len(fields) == 2 {
			break
		}
		// "ACK <sha> common" / "ready"：mult_ack 的中间状态，继续读结论。
	}
	return readPackPayload(r)
}

// readPackPayload 读出结论之后的 pack。
//
// 这里必须两种都认：客户端声明了 side-band-64k，但服务端可能忽略它直接发裸 pack，
// 也可能不管有没有请求都分包（本方旧实现就是这样）。用包头判断而不是靠假设，
// 否则「不支持的格式」会伪装成「不是 pack 数据」。
func readPackPayload(r *bufio.Reader) ([]byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("read pack: %w", err)
	}
	if string(head[:]) == "PACK" {
		// 这里是 pack 本体，不是 ref 广告：用 pack 的上限。
		rest, err := io.ReadAll(io.LimitReader(r, maxPackResponse))
		if err != nil {
			return nil, fmt.Errorf("read pack: %w", err)
		}
		return append(head[:], rest...), nil
	}
	// 否则这 4 字节就是第一条 pkt-line 的长度头。
	return readFramedPack(io.MultiReader(bytes.NewReader(head[:]), r))
}

// readFramedPack 解复用 side-band 分片：0x01 数据、0x02 进度、0x03 错误。
func readFramedPack(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(r)
	var pack []byte
	for {
		line, kind, err := readPkt(br)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read pack chunk: %w", err)
		}
		if kind != pktData {
			if kind == pktFlush || kind == pktResponseEnd {
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
			continue
		case 3:
			return nil, fmt.Errorf("remote reported an error during transfer: %s", strings.TrimSpace(line[1:]))
		default:
			pack = append(pack, line...)
		}
		if len(pack) > maxPackResponse {
			return nil, fmt.Errorf("pack exceeds the %d byte limit", maxPackResponse)
		}
	}
	if len(pack) == 0 {
		return nil, errors.New("remote returned no pack data")
	}
	return pack, nil
}

// localHaves 收集本地已有的 tip，交给远端做「这些我都有」的排除。
//
// 为什么必须同时收集 refs/heads/ 与 refs/remotes/（2026-11-24 实测）：
// 一个纯下游节点（如备份节点、或只读客户端）根本没有本地分支，
// 所有的历史都挂在 refs/remotes/<upstream>/* 下面。只看 heads 的实现
// 会在第二轮 fetch 时声明「我没有任何 have」，导致对端把整条历史
// 又打成一个 pack 发过来——已对齐时短路完全失效，每轮都在全量重传。
//
// 保持在 maxHaves 以内：给太多 have 并不会更省，反而拖长协商。
func localHaves(repo *gitstore.Repo) []string {
	seen := map[string]bool{}
	var out []string
	add := func(prefix string) {
		refs, err := repo.Refs(prefix)
		if err != nil {
			return
		}
		for _, ref := range refs {
			if len(out) >= maxHaves {
				return
			}
			sha, err := repo.Resolve(ref)
			if err != nil || !validSHA(sha) || seen[sha] {
				continue
			}
			seen[sha] = true
			out = append(out, sha)
		}
	}
	add("refs/heads/")
	add("refs/remotes/")
	return out
}

// maxHaves 限制一次协商里声明的 have 数量。
const maxHaves = 64

// applyFetch 把收到的 pack 落盘，并更新远端跟踪引用。
//
// wanted 与 fetchTips 用同一个 branchScope：没要的分支即便被广告了也不写
// refs/remotes/*。这一道不是重复：对象是内容寻址的，一个没要的分支的
// tip 可能因为别的分支引用过它而已经在库里，单靠 HasObject 拦不住——
// 那时就会凭空多出一个从没请求过的跟踪引用。
func applyFetch(repo *gitstore.Repo, remote string, refs map[string]string, pack []byte, wanted []string) (FetchResult, error) {
	res := FetchResult{Branches: map[string]string{}}
	ingested, err := repo.IngestPack(pack)
	if err != nil {
		return res, fmt.Errorf("receive remote objects: %w", err)
	}
	res.Objects = ingested.Objects
	res.Written = ingested.Written

	scope := newBranchScope(wanted)

	for ref, sha := range refs {
		if !strings.HasPrefix(ref, "refs/heads/") || !validSHA(sha) {
			continue
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if !scope.allows(branch) {
			continue
		}
		if !repo.HasObject(sha) {
			// 只广告了没要的东西（比如浅克隆边界）：不写指针，
			// 因为没有对象的 ref 是一个悬空引用。
			continue
		}
		if err := repo.SetRemoteRef(remote, branch, sha); err != nil {
			return res, err
		}
		res.Branches[branch] = sha
	}
	return res, nil
}

// FetchSSH 通过 SSH 取回远端历史。
func FetchSSH(ctx context.Context, repo *gitstore.Repo, remoteURL, remote, privateKeyPEM, hostKey string, branches []string, timeout time.Duration) (FetchResult, error) {
	sess, err := dialSSH(ctx, remoteURL, privateKeyPEM, hostKey, timeout)
	if err != nil {
		return FetchResult{}, err
	}
	defer sess.close()

	// 协议 v2 通过 GIT_PROTOCOL 环境变量开启。服务端不认也无所谓，
	// 广告里会写明它是 v0，后面照 v0 走。
	_ = sess.setenv("GIT_PROTOCOL", "version=2")
	if err := sess.start("git-upload-pack"); err != nil {
		return FetchResult{}, err
	}
	reader := bufio.NewReader(sess.stdout)
	adv, err := readAdvertisement(reader)
	if err != nil {
		return FetchResult{}, fmt.Errorf("read upload-pack advertisement: %w", err)
	}
	if adv.Version == 2 {
		return fetchV2SSH(repo, remote, sess, reader, adv, branches)
	}
	refs := adv.Refs
	tips := fetchTips(refs, branches)
	if len(tips) == 0 {
		return FetchResult{Branches: map[string]string{}}, nil
	}
	if _, err := sess.stdin.Write(fetchBody(tips, localHaves(repo))); err != nil {
		return FetchResult{}, fmt.Errorf("send want: %w", err)
	}
	if err := sess.stdin.Close(); err != nil {
		return FetchResult{}, err
	}
	pack, err := readFetchPack(reader)
	if err != nil {
		return FetchResult{}, err
	}
	if err := sess.wait(); err != nil {
		return FetchResult{}, err
	}
	return applyFetch(repo, remote, refs, pack, branches)
}

// ListSSH 只读广告，不取对象。sync 用它，避免为了比对而下载整段历史。
func ListSSH(ctx context.Context, remoteURL, privateKeyPEM, hostKey string, timeout time.Duration) (map[string]string, error) {
	sess, err := dialSSH(ctx, remoteURL, privateKeyPEM, hostKey, timeout)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	if err := sess.start("git-upload-pack"); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(sess.stdout)
	adv, err := readAdvertisement(reader)
	if err != nil {
		return nil, fmt.Errorf("read upload-pack advertisement: %w", err)
	}
	// v2 的广告里没有 refs，要另发 ls-refs（同 ListHTTP 的理由）。
	if adv.Version == 2 {
		if _, err := sess.stdin.Write(v2LsRefsRequest()); err != nil {
			return nil, fmt.Errorf("send ls-refs: %w", err)
		}
		refs, err := parseV2LsRefs(reader)
		if err != nil {
			return nil, err
		}
		_ = sess.stdin.Close()
		_ = sess.wait()
		return refs, nil
	}
	return adv.Refs, nil
}

// FetchHTTP 通过 smart HTTP 取回远端历史。
func FetchHTTP(ctx context.Context, repo *gitstore.Repo, remoteURL, remote, user, token string, branches []string, timeout time.Duration) (FetchResult, error) {
	base, client, err := httpClientFor(remoteURL, user, token, timeout)
	if err != nil {
		return FetchResult{}, err
	}
	user, token = normalizeHTTPCreds(user, token)
	adv, err := discoverHTTPFor(ctx, client, base, "git-upload-pack", user, token, true)
	if err != nil {
		return FetchResult{}, err
	}
	if adv.Version == 2 {
		return fetchV2HTTP(ctx, client, base, user, token, repo, remote, branches)
	}
	refs := adv.Refs
	tips := fetchTips(refs, branches)
	if len(tips) == 0 {
		return FetchResult{Branches: map[string]string{}}, nil
	}
	pack, err := v0PackHTTP(ctx, client, base, user, token, fetchBody(tips, localHaves(repo)))
	if err != nil {
		return FetchResult{}, err
	}
	return applyFetch(repo, remote, refs, pack, branches)
}

// ListHTTP 只读广告，不取对象。
func ListHTTP(ctx context.Context, remoteURL, user, token string, timeout time.Duration) (map[string]string, error) {
	base, client, err := httpClientFor(remoteURL, user, token, timeout)
	if err != nil {
		return nil, err
	}
	user, token = normalizeHTTPCreds(user, token)
	adv, err := discoverHTTPFor(ctx, client, base, "git-upload-pack", user, token, true)
	if err != nil {
		return nil, err
	}
	// v2 的广告里没有 refs——它把「有哪些 ref」拆成独立的 ls-refs 命令。
	// 直接返回 adv.Refs 会得到一张空表，于是 sync 对任何支持 v2 的服务端
	// 都报 "(no readable branches)"（2026-11-24 测试当场抓到；3060 只回 v0，
	// 所以生产上一直没暴露）。要拿 refs 就得再发一次 ls-refs。
	if adv.Version == 2 {
		body, err := postService(ctx, client, base, user, token, "git-upload-pack", v2LsRefsRequest(), true)
		if err != nil {
			return nil, err
		}
		return parseV2LsRefs(bufio.NewReader(bytes.NewReader(body)))
	}
	return adv.Refs, nil
}

// postService 发一次 stateless-rpc 的 POST，返回原始响应体。
// v2 下同一个端点承载多个命令，靠请求体区分。
func postService(ctx context.Context, client *http.Client, base, user, token, service string, body []byte, wantV2 bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+service, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(user, token)
	req.Header.Set("Content-Type", "application/x-"+service+"-request")
	req.Header.Set("Accept", "application/x-"+service+"-result")
	if wantV2 {
		// 这个头声明本次请求体是 v2 命令；发给一个 v0 服务端会直接报
		// 「unknown capability」。所以它必须跟着请求体一起决定。
		req.Header.Set("Git-Protocol", "version=2")
	}
	req.Header.Set("User-Agent", "zero-memcp")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", redactURL(base), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("remote returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	// packfile 响应可以远大于 ref 广告（取决于仓库体积），所以这里不能
	// 沿用广告的 8MB 上限。超限时明确报错而不是截断：截断只会让上半层的
	// pkt-line 解析器报 "unexpected EOF"，把「本端自己切了」误导成「对端断了」。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPackResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", service, err)
	}
	if int64(len(data)) > maxPackResponse {
		return nil, fmt.Errorf("%s response exceeds the %d byte limit", service, int64(maxPackResponse))
	}
	return data, nil
}

// v0PackHTTP 发送 v0 的 want/have 请求并取回 pack。
func v0PackHTTP(ctx context.Context, client *http.Client, base, user, token string, body []byte) ([]byte, error) {
	raw, err := postService(ctx, client, base, user, token, "git-upload-pack", body, false)
	if err != nil {
		return nil, err
	}
	return readFetchPack(bufio.NewReader(bytes.NewReader(raw)))
}

// fetchV2SSH 走协议 v2：先 ls-refs 拿分支表，再 fetch 取对象。
//
// v2 把「有哪些 ref」和「取哪些对象」拆成两个命令，
// 好处是两次之间可以只针对需要的分支发 want，不必被整份广告牵着走。
func fetchV2SSH(repo *gitstore.Repo, remote string, sess *sshSession, reader *bufio.Reader, adv advertisement, branches []string) (FetchResult, error) {
	if _, ok := adv.Caps["fetch"]; !ok {
		return FetchResult{}, errors.New("server claims protocol v2 but does not support fetch")
	}
	if _, err := sess.stdin.Write(v2LsRefsRequest()); err != nil {
		return FetchResult{}, fmt.Errorf("send ls-refs: %w", err)
	}
	refs, err := parseV2LsRefs(reader)
	if err != nil {
		return FetchResult{}, err
	}
	tips := fetchTips(refs, branches)
	if len(tips) == 0 {
		_ = sess.stdin.Close()
		_ = sess.wait()
		return FetchResult{Branches: map[string]string{}}, nil
	}
	if _, err := sess.stdin.Write(v2FetchRequest(tips, localHaves(repo))); err != nil {
		return FetchResult{}, fmt.Errorf("send fetch: %w", err)
	}
	if err := sess.stdin.Close(); err != nil {
		return FetchResult{}, err
	}
	pack, err := parseV2Pack(reader)
	if err != nil {
		return FetchResult{}, err
	}
	if err := sess.wait(); err != nil {
		return FetchResult{}, err
	}
	return applyFetch(repo, remote, refs, pack, branches)
}

// fetchV2HTTP 走协议 v2 over stateless HTTP：ls-refs 与 fetch 各一次 POST。
func fetchV2HTTP(ctx context.Context, client *http.Client, base, user, token string, repo *gitstore.Repo, remote string, branches []string) (FetchResult, error) {
	refsBody, err := postService(ctx, client, base, user, token, "git-upload-pack", v2LsRefsRequest(), true)
	if err != nil {
		return FetchResult{}, err
	}
	refs, err := parseV2LsRefs(bufio.NewReader(bytes.NewReader(refsBody)))
	if err != nil {
		return FetchResult{}, err
	}
	tips := fetchTips(refs, branches)
	if len(tips) == 0 {
		return FetchResult{Branches: map[string]string{}}, nil
	}
	packBody, err := postService(ctx, client, base, user, token, "git-upload-pack", v2FetchRequest(tips, localHaves(repo)), true)
	if err != nil {
		return FetchResult{}, err
	}
	pack, err := parseV2Pack(bufio.NewReader(bytes.NewReader(packBody)))
	if err != nil {
		return FetchResult{}, err
	}
	return applyFetch(repo, remote, refs, pack, branches)
}
