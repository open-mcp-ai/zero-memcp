package server

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/gittransport"
)

// HTTP push 的闭环：root 存 token → push 走内置 smart-HTTP 客户端 →
// 远端 ref 真的前进。远端仍由系统 git 扮演（那是被推的一方）。
func TestHTTPPushThroughServerVerbs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remoteDir)

	hs := newSmartHTTP(t, remoteDir, "deploy", "ghp_token_value")
	defer hs.Close()

	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project    = "daily"
name       = "github"
url        = %q
push       = ["zero"]
http_user  = "deploy"
http_token = "gh"
`, dataDir, hs.URL+"/repo.git")
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "n.md", []byte("备忘\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Commit("zero", "seed")
	if err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})
	roTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "ro", Cap: "r"})

	out, err := callMCPWithVars(t, srv, rootTok, "remote-secret put gh -- $TOKEN", map[string]string{"TOKEN": "ghp_token_value"})
	if err != nil {
		t.Fatalf("remote-secret put 失败: %v", err)
	}
	if strings.Contains(out, "ghp_token_value") {
		t.Fatalf("token 被回显: %s", out)
	}
	if _, err := callMCPWithVars(t, srv, roTok, "remote-secret put x -- $TOKEN", map[string]string{"TOKEN": "v"}); err == nil {
		t.Fatal("只读凭证不该能写 token")
	}

	if _, err := callMCP(t, srv, rootTok, "push daily github zero"); err != nil {
		t.Fatalf("HTTP push 失败: %v", err)
	}
	if got := strings.TrimSpace(gitOutput(t, remoteDir, "rev-parse", "refs/heads/zero")); got != head {
		t.Fatalf("远端应推进到 %s，实际 %s", head, got)
	}

	// token 文件必须 0600。
	st, err := os.Stat(filepath.Join(dataDir, "system", "secrets", "gh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token 文件权限 %04o，应为 0600", st.Mode().Perm())
	}
}

// 凭证名与传输方式不匹配必须在配置解析期失败，不能等到 push 才发现。
func TestRemoteCredentialKindMustMatchScheme(t *testing.T) {
	base := `
[server]
data = "/tmp/d"

[[user]]
name = "zero"
root = true

[[project]]
name  = "p"
owner = "zero"
mode  = "rwx------"

[[remote]]
project = "p"
name    = "r"
push    = ["zero"]
`
	sshKeyOnHTTP := base + "url = \"https://example.invalid/p.git\"\nssh_key = \"k\"\n"
	if _, err := config.ParseFile([]byte(sshKeyOnHTTP)); err == nil {
		t.Fatal("ssh_key 配到 https 远端应报错")
	}
	httpTokenOnSSH := base + "url = \"git@example.invalid:p.git\"\nhttp_token = \"t\"\n"
	if _, err := config.ParseFile([]byte(httpTokenOnSSH)); err == nil {
		t.Fatal("http_token 配到 SSH 远端应报错")
	}
}

// 私钥只用于 SSH；存了私钥也不能被 HTTP 顺手拿去当 token。
func TestRemoteKeyRejectsWrongPEM(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})
	_, err := callMCPWithVars(t, srv, tok, "remote-key put bad -- $KEY", map[string]string{"KEY": "not a key"})
	if err == nil {
		t.Fatal("非私钥内容应被拒绝")
	}
}

func newSmartHTTP(t *testing.T, repoDir, user, token string) *httptest.Server {
	t.Helper()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != want {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		body := forwardGit(t, repoDir, "info/refs", r)
		_, _ = w.Write(body)
	}))
	mux.HandleFunc("/repo.git/git-receive-pack", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		_, _ = w.Write(forwardGit(t, repoDir, "git-receive-pack", r))
	}))
	return httptest.NewServer(mux)
}

func forwardGit(t *testing.T, repoDir, action string, r *http.Request) []byte {
	t.Helper()
	args := []string{"receive-pack", "--stateless-rpc"}
	if action == "info/refs" {
		args = append(args, "--advertise-refs")
	}
	args = append(args, repoDir)
	cmd := exec.Command("git", args...)
	cmd.Stdin = r.Body
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	if action == "info/refs" {
		return append([]byte("001f# service=git-receive-pack\n0000"), out...)
	}
	return out
}

// 本地路径远端的闭环：fetch 取回、push 推进，双方都不经过系统 git 协议层。
func TestLocalPathRemoteFetchAndPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "backup.git")
	runGit(t, t.TempDir(), "init", "--bare", remoteDir)

	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project = "daily"
name    = "backup"
url     = %q
push    = ["zero"]
`, dataDir, remoteDir)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "a.md", []byte("内容\n"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Commit("zero", "seed")
	if err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{DataDir: dataDir, ConfigFile: filepath.Join(dataDir, "zero-memcp.toml"), Listen: "127.0.0.1:0", EnforceAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "owner", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "push daily backup zero"); err != nil {
		t.Fatalf("本地路径 push 失败: %v", err)
	}
	if got := strings.TrimSpace(gitOutput(t, remoteDir, "rev-parse", "refs/heads/zero")); got != head {
		t.Fatalf("远端应推进到 %s，实际 %s", head, got)
	}

	out, err := callMCP(t, srv, tok, "sync daily backup")
	if err != nil {
		t.Fatalf("sync 失败: %v", err)
	}
	if !strings.Contains(out, "synced") {
		t.Fatalf("刚推完应是 synced: %s", out)
	}

	// 反向：本地新开一个仓库节点，从 backup 取回。
	other := t.TempDir()
	otherRepo, err := gitstore.Init(filepath.Join(other, "daily.git"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := gittransport.FetchLocal(otherRepo, remoteDir, "backup")
	if err != nil {
		t.Fatalf("本地 fetch 失败: %v", err)
	}
	if res.Branches["zero"] != head {
		t.Fatalf("取回分支不符: %v", res.Branches)
	}
	if !otherRepo.HasObject(head) {
		t.Fatal("取回后对象应可读")
	}
}
