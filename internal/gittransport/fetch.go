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

// fetchTips 从广告里挑出要取的分支。
func fetchTips(refs map[string]string) []string {
	tips := make([]string, 0, len(refs))
	for ref, sha := range refs {
		if strings.HasPrefix(ref, "refs/heads/") && validSHA(sha) {
			tips = append(tips, sha)
		}
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
			return nil, fmt.Errorf("读取协商响应失败: %w", err)
		}
		if kind == pktFlush || kind == pktDelim || kind == pktResponseEnd {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "ERR ") {
			return nil, fmt.Errorf("远端拒绝取回: %s", strings.TrimPrefix(trimmed, "ERR "))
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
		return nil, fmt.Errorf("读取 pack 失败: %w", err)
	}
	if string(head[:]) == "PACK" {
		rest, err := io.ReadAll(io.LimitReader(r, maxAdvertisement))
		if err != nil {
			return nil, fmt.Errorf("读取 pack 失败: %w", err)
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
			return nil, fmt.Errorf("读取 pack 分片失败: %w", err)
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
			return nil, fmt.Errorf("远端在传输中报错: %s", strings.TrimSpace(line[1:]))
		default:
			pack = append(pack, line...)
		}
		if len(pack) > maxAdvertisement {
			return nil, fmt.Errorf("pack 超过 %d 字节上限", maxAdvertisement)
		}
	}
	if len(pack) == 0 {
		return nil, errors.New("远端没有返回 pack 数据")
	}
	return pack, nil
}

// localHaves 收集本地已有的 tip，交给远端做「这些我都有」的排除。
//
// 只取分支尖：给太多 have 并不会更省，反而拖长协商；
// 而少给几个只会多收一点对象，不会出错。
func localHaves(repo *gitstore.Repo) []string {
	refs, err := repo.Refs("refs/heads/")
	if err != nil {
		return nil
	}
	if len(refs) > maxHaves {
		refs = refs[:maxHaves]
	}
	var out []string
	for _, ref := range refs {
		if sha, err := repo.Resolve(ref); err == nil && validSHA(sha) {
			out = append(out, sha)
		}
	}
	return out
}

// maxHaves 限制一次协商里声明的 have 数量。
const maxHaves = 64

// applyFetch 把收到的 pack 落盘，并更新远端跟踪引用。
func applyFetch(repo *gitstore.Repo, remote string, refs map[string]string, pack []byte) (FetchResult, error) {
	res := FetchResult{Branches: map[string]string{}}
	ingested, err := repo.IngestPack(pack)
	if err != nil {
		return res, fmt.Errorf("接收远端对象失败: %w", err)
	}
	res.Objects = ingested.Objects
	res.Written = ingested.Written

	for ref, sha := range refs {
		if !strings.HasPrefix(ref, "refs/heads/") || !validSHA(sha) {
			continue
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
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
	if len(res.Branches) == 0 && len(refs) > 0 {
		return res, errors.New("远端广告了分支，但取回后没有任何可落地的引用")
	}
	return res, nil
}

// FetchSSH 通过 SSH 取回远端历史。
func FetchSSH(ctx context.Context, repo *gitstore.Repo, remoteURL, remote, privateKeyPEM, hostKey string, timeout time.Duration) (FetchResult, error) {
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
		return FetchResult{}, fmt.Errorf("读取 upload-pack 广告失败: %w", err)
	}
	if adv.Version == 2 {
		return fetchV2SSH(repo, remote, sess, reader, adv)
	}
	refs := adv.Refs
	tips := fetchTips(refs)
	if len(tips) == 0 {
		return FetchResult{Branches: map[string]string{}}, nil
	}
	if _, err := sess.stdin.Write(fetchBody(tips, localHaves(repo))); err != nil {
		return FetchResult{}, fmt.Errorf("发送 want 失败: %w", err)
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
	return applyFetch(repo, remote, refs, pack)
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
	adv, err := readAdvertisement(bufio.NewReader(sess.stdout))
	if err != nil {
		return nil, fmt.Errorf("读取 upload-pack 广告失败: %w", err)
	}
	return adv.Refs, nil
}

// FetchHTTP 通过 smart HTTP 取回远端历史。
func FetchHTTP(ctx context.Context, repo *gitstore.Repo, remoteURL, remote, user, token string, timeout time.Duration) (FetchResult, error) {
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
		return fetchV2HTTP(ctx, client, base, user, token, repo, remote)
	}
	refs := adv.Refs
	tips := fetchTips(refs)
	if len(tips) == 0 {
		return FetchResult{Branches: map[string]string{}}, nil
	}
	pack, err := v0PackHTTP(ctx, client, base, user, token, fetchBody(tips, localHaves(repo)))
	if err != nil {
		return FetchResult{}, err
	}
	return applyFetch(repo, remote, refs, pack)
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
		return nil, fmt.Errorf("连接 %s 失败: %w", redactURL(base), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("远端返回 %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxAdvertisement))
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
func fetchV2SSH(repo *gitstore.Repo, remote string, sess *sshSession, reader *bufio.Reader, adv advertisement) (FetchResult, error) {
	if _, ok := adv.Caps["fetch"]; !ok {
		return FetchResult{}, errors.New("服务端声称 v2 但没有 fetch 能力")
	}
	if _, err := sess.stdin.Write(v2LsRefsRequest()); err != nil {
		return FetchResult{}, fmt.Errorf("发送 ls-refs 失败: %w", err)
	}
	refs, err := parseV2LsRefs(reader)
	if err != nil {
		return FetchResult{}, err
	}
	tips := fetchTips(refs)
	if len(tips) == 0 {
		_ = sess.stdin.Close()
		_ = sess.wait()
		return FetchResult{Branches: map[string]string{}}, nil
	}
	if _, err := sess.stdin.Write(v2FetchRequest(tips, localHaves(repo))); err != nil {
		return FetchResult{}, fmt.Errorf("发送 fetch 失败: %w", err)
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
	return applyFetch(repo, remote, refs, pack)
}

// fetchV2HTTP 走协议 v2 over stateless HTTP：ls-refs 与 fetch 各一次 POST。
func fetchV2HTTP(ctx context.Context, client *http.Client, base, user, token string, repo *gitstore.Repo, remote string) (FetchResult, error) {
	refsBody, err := postService(ctx, client, base, user, token, "git-upload-pack", v2LsRefsRequest(), true)
	if err != nil {
		return FetchResult{}, err
	}
	refs, err := parseV2LsRefs(bufio.NewReader(bytes.NewReader(refsBody)))
	if err != nil {
		return FetchResult{}, err
	}
	tips := fetchTips(refs)
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
	return applyFetch(repo, remote, refs, pack)
}
