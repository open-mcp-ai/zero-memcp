package gittransport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// maxAdvertisement 限制一次 ref 广告的读取量。
// 一个错误的地址（比如把人家的 HTML 页面当仓库）不该把内存读穿。
// maxAdvertisement 是 ref 广告的上限。一条广告是一行一个分支，
// 8MB 足够放下几万个分支，超出说明对端不是在说 git 协议。
const maxAdvertisement = 8 << 20

// maxPackResponse 是 packfile 响应的上限。
//
// 这里曾是 maxAdvertisement，于是「取一个大于 8MB 的仓库」永远失败：
// 响应被 LimitReader 静默截断，下游按 pkt-line 解析时读到半个分片，
// 报 "read pack chunk: unexpected EOF"——一个完全指错方向的错误，
// 看起来像对端断线，实际是本端自己截的。
//
// 用与接收端一致的上限，并且超限时明确报错，不再静默截断。
const maxPackResponse = 512 << 20

// PushHTTP 通过 smart HTTP 推送一个分支。
//
// 与 SSH 版共用同一套判定：非快进拒绝、必须拿到 report-status 才算成功。
// 认证只走 HTTP Basic，凭证由调用方从服务端密钥库取出后传入，
// 绝不拼进 URL——URL 会出现在日志和错误信息里。
func PushHTTP(ctx context.Context, repo *gitstore.Repo, remoteURL, branch, user, token string, timeout time.Duration) (PushResult, error) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := validateBranch(branch); err != nil {
		return PushResult{}, err
	}
	base, client, err := httpClientFor(remoteURL, user, token, timeout)
	if err != nil {
		return PushResult{}, err
	}
	user, token = normalizeHTTPCreds(user, token)

	adv, err := discoverHTTPFor(ctx, client, base, "git-receive-pack", user, token, false)
	if err != nil {
		return PushResult{}, err
	}
	refs := adv.Refs
	if !adv.Caps["report-status"] {
		return PushResult{}, errors.New("remote receive-pack does not support report-status; refusing to push without verifying the result")
	}

	plan, err := planPush(repo, branch, refs)
	if err != nil {
		return PushResult{}, err
	}
	if plan.UpToDate {
		return plan.Result(), nil
	}

	req := &pushRequest{
		repo:   repo,
		branch: branch,
		oldSHA: plan.Old,
		newSHA: plan.New,
	}
	body, err := req.body()
	if err != nil {
		return PushResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/git-receive-pack", bytes.NewReader(body))
	if err != nil {
		return PushResult{}, err
	}
	httpReq.SetBasicAuth(user, token)
	httpReq.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	httpReq.Header.Set("Accept", "application/x-git-receive-pack-result")
	httpReq.Header.Set("User-Agent", "zero-memcp")

	resp, err := client.Do(httpReq)
	if err != nil {
		return PushResult{}, fmt.Errorf("connect to %s: %w", redactURL(base), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return PushResult{}, fmt.Errorf("remote returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	status, err := readReportStatus(bufio.NewReader(io.LimitReader(resp.Body, maxAdvertisement)))
	if err != nil {
		return PushResult{}, fmt.Errorf("remote did not confirm push: %w", err)
	}
	if err := checkReportStatus(status, branch); err != nil {
		return PushResult{}, err
	}
	return plan.Result(), nil
}

// httpClientFor 校验远端并造出客户端。
func httpClientFor(remoteURL, user, token string, timeout time.Duration) (string, *http.Client, error) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	base, err := parseHTTPRemote(remoteURL)
	if err != nil {
		return "", nil, err
	}
	// 允许匿名：公开仓库不需要凭证，硬要一个 token 会让「读一个开源仓库」
	// 变成做不到的事。私有仓库缺凭证会由服务端 401 说话，那时的错误更准确。
	return base, &http.Client{
		Timeout: timeout,
		// 不跟随重定向：把 Authorization 带去别的主机等于泄漏凭证。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func normalizeHTTPCreds(user, token string) (string, string) {
	if user == "" {
		user = "git"
	}
	return user, token
}

// discoverHTTPFor 读取某个 service 的 info/refs 广告。
func discoverHTTPFor(ctx context.Context, client *http.Client, base, service, user, token string, wantV2 bool) (advertisement, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/info/refs?service="+service, nil)
	if err != nil {
		return advertisement{}, err
	}
	req.SetBasicAuth(user, token)
	if wantV2 {
		// 广告也要声明版本，否则服务端会按 v0 回广告，后面就一直是 v0 了。
		req.Header.Set("Git-Protocol", "version=2")
	}
	req.Header.Set("Accept", "application/x-"+service+"-advertisement")
	req.Header.Set("User-Agent", "zero-memcp")

	resp, err := client.Do(req)
	if err != nil {
		return advertisement{}, fmt.Errorf("connect to %s: %w", redactURL(base), err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return advertisement{}, errors.New("remote rejected credentials (401): check http_user / http_token")
	case http.StatusForbidden:
		return advertisement{}, fmt.Errorf("remote rejected %s (403): credentials are valid but lack permission", service)
	case http.StatusNotFound:
		return advertisement{}, fmt.Errorf("remote repository does not exist: %s", redactURL(base))
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return advertisement{}, fmt.Errorf("remote returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return readAdvertisement(bufio.NewReader(io.LimitReader(resp.Body, maxAdvertisement)))
}

// parseHTTPRemote 校验并规范化远端地址。
func parseHTTPRemote(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse HTTP remote: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("not an HTTP(S) remote: %s", redactURL(raw))
	}
	if u.User != nil {
		return "", errors.New("remote URL must not embed credentials; store the token with remote-secret")
	}
	if u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return "", fmt.Errorf("HTTP remote is missing a host or repository path: %s", redactURL(raw))
	}
	// token 默认只能走 TLS。
	//
	// 明文 HTTP 的两个例外：
	// 1. 回环地址（127.0.0.1 / ::1 / localhost）：本地测试与自托管同机代理。
	// 2. 显式带有 ?insecure=1 查询参数的 URL：明确在局域网 / WireGuard /
	//    EasyTier 等加密覆盖网内运行的私有端点。
	//
	// 为什么要求显式声明：在公共 WiFi 等不受控网络下，纯私网 IP（192.168.x）
	// 也可能被嗅探。写配置的人必须在 URL 尾部显式声明 ?insecure=1，
	// 把「这是安全网格」的承诺钉在配置里。公网 IP/域名带此参数也允许，
	// 但由写配置的人承担明文泄漏风险（责任边界清晰）。
	//
	// 规范化输出时把该 query 参数剥离，不污染实际发往对端的 HTTP 请求路径。
	insecure := u.Query().Get("insecure") == "1"
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) && !insecure {
		return "", fmt.Errorf("refusing to send credentials over plain HTTP (%s); use https:// or SSH (or append ?insecure=1 for mesh networks)", u.Hostname())
	}
	q := u.Query()
	q.Del("insecure")
	u.RawQuery = q.Encode()
	return strings.TrimSuffix(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable remote>"
	}
	u.User = nil
	return u.String()
}

// checkReportStatus 把 report-status 翻成错误或成功。
func checkReportStatus(status map[string]string, branch string) error {
	if status["unpack"] != "ok" {
		return fmt.Errorf("remote unpack failed: %s", status["unpack"])
	}
	if got := status["refs/heads/"+branch]; got != "ok" {
		return fmt.Errorf("remote rejected update to refs/heads/%s: %s", branch, got)
	}
	return nil
}
