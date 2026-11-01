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
const maxAdvertisement = 8 << 20

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
		return PushResult{}, errors.New("远端 receive-pack 不支持 report-status；拒绝在无法核实结果时上传")
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
		return PushResult{}, fmt.Errorf("连接 %s 失败: %w", redactURL(base), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return PushResult{}, fmt.Errorf("远端返回 %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	status, err := readReportStatus(bufio.NewReader(io.LimitReader(resp.Body, maxAdvertisement)))
	if err != nil {
		return PushResult{}, fmt.Errorf("远端未确认 push: %w", err)
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
		return advertisement{}, fmt.Errorf("连接 %s 失败: %w", redactURL(base), err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return advertisement{}, errors.New("远端拒绝了凭证（401）：检查 http_user / http_token")
	case http.StatusForbidden:
		return advertisement{}, fmt.Errorf("远端拒绝 %s（403）：凭证有效但缺权限", service)
	case http.StatusNotFound:
		return advertisement{}, fmt.Errorf("远端没有这个仓库：%s", redactURL(base))
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return advertisement{}, fmt.Errorf("远端返回 %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return readAdvertisement(bufio.NewReader(io.LimitReader(resp.Body, maxAdvertisement)))
}

// parseHTTPRemote 校验并规范化远端地址。
func parseHTTPRemote(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("解析 HTTP 远端失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("不是 HTTP(S) 远端：%s", redactURL(raw))
	}
	if u.User != nil {
		return "", errors.New("远端地址不能内嵌凭证：用 remote-secret 保存 token")
	}
	if u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return "", fmt.Errorf("HTTP 远端缺少主机或仓库路径：%s", redactURL(raw))
	}
	// token 只能走 TLS。明文 HTTP 只允许回环地址（本机测试/自托管代理）。
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("拒绝用明文 HTTP 发送凭证（%s）：改用 https:// 或 SSH", u.Hostname())
	}
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
		return "<无法解析的远端>"
	}
	u.User = nil
	return u.String()
}

// checkReportStatus 把 report-status 翻成错误或成功。
func checkReportStatus(status map[string]string, branch string) error {
	if status["unpack"] != "ok" {
		return fmt.Errorf("远端解包失败: %s", status["unpack"])
	}
	if got := status["refs/heads/"+branch]; got != "ok" {
		return fmt.Errorf("远端拒绝更新 refs/heads/%s: %s", branch, got)
	}
	return nil
}
