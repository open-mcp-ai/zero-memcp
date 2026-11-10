package gittransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// smartHTTPGit 是一个最小的 smart-HTTP receive-pack 服务端：
// 只用 Basic 认证校验身份，然后把请求转给真正的 git receive-pack。
func smartHTTPGit(t *testing.T, repoDir, user, token string) *httptest.Server {
	t.Helper()
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != auth {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", guard(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("service"); got != "git-receive-pack" {
			http.Error(w, "unsupported service "+got, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		w.Write(advertiseAndForward(t, repoDir, "info/refs", r))
	}))
	mux.HandleFunc("/repo.git/git-receive-pack", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Write(advertiseAndForward(t, repoDir, "git-receive-pack", r))
	}))
	return httptest.NewServer(mux)
}

// advertiseAndForward 调用系统 git 完成实际的 receive-pack 协议，
// 它扮演的是「远端」（GitHub/GitLab 那一侧），不是客户端。
func advertiseAndForward(t *testing.T, repoDir, action string, r *http.Request) []byte {
	t.Helper()
	args := []string{"receive-pack", "--stateless-rpc"}
	if action == "info/refs" {
		args = append(args, "--advertise-refs")
	}
	args = append(args, repoDir)
	cmd := exec.Command("git", args...)
	cmd.Stdin = r.Body
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	if action == "info/refs" {
		// smart HTTP 要求在广告前先发一条 service 声明。
		head := "001f# service=git-receive-pack\n0000"
		return append([]byte(head), out...)
	}
	return out
}

func TestHTTPPushEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)

	srv := smartHTTPGit(t, remoteDir, "deploy", "s3cr3t")
	defer srv.Close()

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write("main", "memory.md", []byte("durable memory\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	head, err := local.Commit("main", "initial memory")
	if err != nil {
		t.Fatal(err)
	}

	res, err := PushHTTP(context.Background(), local, srv.URL+"/repo.git", "main", "deploy", "s3cr3t", 15*time.Second)
	if err != nil {
		t.Fatalf("HTTP push 失败: %v", err)
	}
	if res.New != head {
		t.Fatalf("result %s != %s", res.New, head)
	}
	out := gitRemoteOutput(t, remoteDir, "rev-parse", "refs/heads/main")
	if out != head {
		t.Fatalf("远端 head=%s want=%s", out, head)
	}

	// 再推一次：远端已是最新，必须报「无需更新」而不是重复上传。
	again, err := PushHTTP(context.Background(), local, srv.URL+"/repo.git", "main", "deploy", "s3cr3t", 15*time.Second)
	if err != nil {
		t.Fatalf("重复 push 失败: %v", err)
	}
	if again.Old != head || again.New != head {
		t.Fatalf("重复 push 应识别为已同步: %+v", again)
	}
}

func TestHTTPPushRejectsBadCredentials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	runTestGit(t, "init", "--bare", remoteDir)
	srv := smartHTTPGit(t, remoteDir, "deploy", "right")
	defer srv.Close()

	local, err := gitstore.Init(filepath.Join(root, "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = local.Write("main", "a.md", []byte("a"), gitstore.ModeContent)
	_, _ = local.Commit("main", "seed")

	_, err = PushHTTP(context.Background(), local, srv.URL+"/repo.git", "main", "deploy", "wrong", 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误凭证应被拒绝并说明原因: %v", err)
	}
}

// 明文 HTTP 只允许回环：其余主机必须用 https，否则 token 就是明文广播。
func TestHTTPPushRefusesCleartextToRemoteHost(t *testing.T) {
	local, err := gitstore.Init(filepath.Join(t.TempDir(), "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	_ = local.Write("main", "a.md", []byte("a"), gitstore.ModeContent)
	_, _ = local.Commit("main", "seed")

	_, err = PushHTTP(context.Background(), local, "http://git.example.com/repo.git", "main", "u", "tok", time.Second)
	if err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("应拒绝向非回环主机明文发送凭证: %v", err)
	}
}

func TestHTTPPushRejectsEmbeddedCredentials(t *testing.T) {
	local, err := gitstore.Init(filepath.Join(t.TempDir(), "local.git"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PushHTTP(context.Background(), local, "https://u:p@example.com/repo.git", "main", "", "", time.Second); err == nil {
		t.Fatal("URL 内嵌凭证应被拒绝")
	}
}

func gitRemoteOutput(t *testing.T, repoDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir", repoDir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

var _ = url.Parse

// ?insecure=1 是明文 HTTP 的显式放行：写配置的人以此声明「这条链路在
// 加密覆盖网内」（WireGuard / EasyTier / Tailscale 之类）。
//
// 两件事必须同时成立：
//  1. 不带该参数时，非回环地址依然被拒（公网与陌生内网都不放过）。
//  2. 带该参数时放行，但规范化后的 URL 里不能残留 insecure 查询参数——
//     它只是本端的许可标记，不该出现在实际请求路径上。
func TestHTTPPushInsecureFlagAllowsPlaintextToRemoteHost(t *testing.T) {
	if _, err := parseHTTPRemote("http://10.10.10.12:1729/repos/x/y.git"); err == nil {
		t.Fatal("不带 ?insecure=1 时应拒绝向非回环主机明文发送凭证")
	}
	got, err := parseHTTPRemote("http://10.10.10.12:1729/repos/x/y.git?insecure=1")
	if err != nil {
		t.Fatalf("带 ?insecure=1 时应放行: %v", err)
	}
	if strings.Contains(got, "insecure") {
		t.Fatalf("规范化后不应残留 insecure 参数: %s", got)
	}
	if want := "http://10.10.10.12:1729/repos/x/y.git"; got != want {
		t.Fatalf("规范化结果应为 %q，得到 %q", want, got)
	}
}

// 回环地址天然放行，不需要任何标记——本机测试与自托管同机代理都靠它。
func TestParseHTTPRemoteLoopbackNeedsNoFlag(t *testing.T) {
	got, err := parseHTTPRemote("http://127.0.0.1:1729/repos/x/y.git")
	if err != nil {
		t.Fatalf("回环地址不应需要 insecure 标记: %v", err)
	}
	if got != "http://127.0.0.1:1729/repos/x/y.git" {
		t.Fatalf("回环地址规范化结果不对: %q", got)
	}
}

// 大于 8MB 的 packfile 必须能取回。
//
// 回归的是一个把「ref 广告上限」当成「pack 上限」的错误：广告是几行文本，
// 8MB 绰绰有余；pack 的大小取决于仓库体积，20~30MB 的仓库很常见。上限用错
// 直接的后果是响应被 LimitReader 静默截断，下游按 pkt-line 读到半个分片，
// 报 "read pack chunk: unexpected EOF"——把「本端自己切了」伪装成「对端断了」。
func TestFetchReadsPackLargerThanAdvertisementLimit(t *testing.T) {
	// pkt-line 单帧上限约 64KB，要测 >8MB 必须拼多个分片（side-band channel=1）。
	chunkPayload := bytes.Repeat([]byte("z"), 32*1024)
	// 4 字节十六进制长度 + 1 字节通道标识 (0x01) + 载荷
	lineLen := 4 + 1 + len(chunkPayload)
	chunk := make([]byte, lineLen)
	copy(chunk[:4], fmt.Sprintf("%04x", lineLen))
	chunk[4] = 1 // sideband data channel
	copy(chunk[5:], chunkPayload)

	var buf bytes.Buffer
	// 300 个 32KB 分片 = ~9.6MB，明确跨过 8MB 原门槛
	for i := 0; i < 300; i++ {
		buf.Write(chunk)
	}
	buf.WriteString("0000") // flush

	pack, err := readFramedPack(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("大于 8MB 的 pack 应能读回: %v", err)
	}
	if len(pack) != 300*len(chunkPayload) {
		t.Fatalf("读回字节数不对: got %d want %d", len(pack), 300*len(chunkPayload))
	}
}
