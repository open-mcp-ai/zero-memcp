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
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 取回哪些分支必须由配置决定，不能由对端广告了什么决定。
//
// 为什么这条不变量必须存在（2026-11-24 3060 链路卡死的真因）：fetch 原本把
// advertisement 里所有 refs/heads/* 都塞进 want，于是 weft 的 meta.index
// 也被拉——那是每台机器各自重建的索引快照，本机 98 个 blob / 246 MB。
// 打包在 90 秒预算里跑不完，报出来的是「读 git-upload-pack 响应超时」，
// 看起来像网络故障，实际是自己在取一份根本不该跨机同步的缓存。
//
// 这里让远端持有一条只在那边存在的 meta.index 记忆：如果主机取了它，
// 那条记忆的对象就会出现在主机对象库里。按配置只取 main.record 时它必须不出现。
func TestFetchTakesOnlyConfiguredBranches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remoteDir)

	// ── 远端造两条分支：main.record 与 meta.index ──
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "init", work)
	runGit(t, work, "checkout", "-b", "main.record")
	if err := os.WriteFile(filepath.Join(work, "record.md"), []byte("要同步的记忆\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "record")
	runGit(t, work, "push", remoteDir, "main.record")

	runGit(t, work, "checkout", "-b", "meta.index")
	if err := os.WriteFile(filepath.Join(work, "index.json"), []byte("只属于远端的索引快照\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "index snapshot")
	runGit(t, work, "push", remoteDir, "meta.index")

	headRecord := revParse(t, work, "main.record")
	headMeta := revParse(t, work, "meta.index")

	hs := newUploadHTTP(t, remoteDir, "deploy", "ghp_token_value")
	defer hs.Close()

	// 配置里只声明 push（pull 为空会回落到 push），meta.index 不在其中。
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
push       = ["main.record"]
http_user  = "deploy"
http_token = "gh"
`, dataDir, hs.URL+"/repo.git")
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(dataDir, "repos", "daily.git")
	if _, err := gitstore.Init(localDir); err != nil {
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
	if _, err := callMCPWithVars(t, srv, rootTok, "remote-secret put gh -- $TOKEN", map[string]string{"TOKEN": "ghp_token_value"}); err != nil {
		t.Fatalf("remote-secret put 失败: %v", err)
	}

	out, err := callMCP(t, srv, rootTok, "fetch daily github")
	if err != nil {
		t.Fatalf("fetch 失败: %v", err)
	}

	// ① 报出来的只有配置里的那条分支。
	if !strings.Contains(out, "main.record") {
		t.Fatalf("fetch 结果应包含 main.record: %s", out)
	}
	if strings.Contains(out, "meta.index") {
		t.Fatalf("meta.index 没配置，绝不该出现在 fetch 结果里: %s", out)
	}

	repo, err := gitstore.Open(localDir)
	if err != nil {
		t.Fatal(err)
	}

	// ② 要的分支真的落地了。
	if !repo.HasObject(headRecord) {
		t.Fatal("main.record 的 head 应在本地对象库里")
	}
	if got := repo.RemoteRef("github", "main.record"); got != headRecord {
		t.Fatalf("refs/remotes/github/main.record = %q, want %q", got, headRecord)
	}

	// ③ 没要的分支，对象与指针都绝不落地。
	if repo.HasObject(headMeta) {
		t.Fatalf("未配置的 meta.index head %s 竟然被拉进了本地对象库：取回范围没有被配置约束", headMeta)
	}
	if got := repo.RemoteRef("github", "meta.index"); got != "" {
		t.Fatalf("refs/remotes/github/meta.index 不该存在，实际 %q", got)
	}
}

// 反向对照：同一个远端，两条分支都在配置里时，两条都要取回来。
//
// 没有这一条，上面那个测试在「fetch 整体坏掉」时也会通过——
// 一个什么都不拉的实现同样满足「没要的没拉」。
func TestFetchTakesEveryConfiguredBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remoteDir)

	work := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "init", work)
	runGit(t, work, "checkout", "-b", "main.record")
	if err := os.WriteFile(filepath.Join(work, "a.md"), []byte("甲\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "a")
	runGit(t, work, "push", remoteDir, "main.record")

	runGit(t, work, "checkout", "-b", "meta.index")
	if err := os.WriteFile(filepath.Join(work, "b.md"), []byte("乙\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "b")
	runGit(t, work, "push", remoteDir, "meta.index")
	headMeta := revParse(t, work, "meta.index")

	hs := newUploadHTTP(t, remoteDir, "deploy", "ghp_token_value")
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
push       = ["main.record", "meta.index"]
http_user  = "deploy"
http_token = "gh"
`, dataDir, hs.URL+"/repo.git")
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(dataDir, "repos", "daily.git")
	if _, err := gitstore.Init(localDir); err != nil {
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
	if _, err := callMCPWithVars(t, srv, rootTok, "remote-secret put gh -- $TOKEN", map[string]string{"TOKEN": "ghp_token_value"}); err != nil {
		t.Fatalf("remote-secret put 失败: %v", err)
	}
	if _, err := callMCP(t, srv, rootTok, "fetch daily github"); err != nil {
		t.Fatalf("fetch 失败: %v", err)
	}

	repo, err := gitstore.Open(localDir)
	if err != nil {
		t.Fatal(err)
	}
	if !repo.HasObject(headMeta) {
		t.Fatal("meta.index 也在配置里，应该被拉下来")
	}
	if got := repo.RemoteRef("github", "meta.index"); got != headMeta {
		t.Fatalf("refs/remotes/github/meta.index = %q, want %q", got, headMeta)
	}
}

// 两边都没配置的远端不许猜：取回任何东西都是猜的，必须当场报错点名。
func TestFetchWithoutConfiguredBranchesFailsLoudly(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project = "daily"
name    = "github"
url     = "http://127.0.0.1:1/repo.git"
`
	cfg = strings.Replace(cfg, "__DATA__", dataDir, 1)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git")); err != nil {
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
	_, err = callMCP(t, srv, rootTok, "fetch daily github")
	if err == nil {
		t.Fatal("没声明 pull/push 的远端不该被猜着取回，应当报错")
	}
	if !strings.Contains(err.Error(), "no pull branches") {
		t.Fatalf("错误应点名缺配置，实际: %v", err)
	}
}

// revParse 在工作区仓库里取某个 ref 的 sha。
// 不能用 gitOutput：那个 helper 走 --git-dir，只适用于裸仓库。
func revParse(t *testing.T, work, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", work, "rev-parse", ref)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v: %s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}

// push_only 的远端连「兼容性猜测」都不该发生：它永远不取回任何东西。
func TestFetchRefusesPushOnlyRemote(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"

[[remote]]
project   = "daily"
name      = "backup"
url       = "http://127.0.0.1:1/repo.git"
push      = ["main"]
push_only = true
`
	cfg = strings.Replace(cfg, "__DATA__", dataDir, 1)
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := gitstore.Init(filepath.Join(dataDir, "repos", "daily.git")); err != nil {
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
	_, err = callMCP(t, srv, rootTok, "fetch daily backup")
	if err == nil {
		t.Fatal("push_only 远端不该能取回，应当报错")
	}
	if !strings.Contains(err.Error(), "push_only") {
		t.Fatalf("错误应点名 push_only，实际: %v", err)
	}
}

// newUploadHTTP 起一个只提供 git-upload-pack（取回方向）的 smart HTTP 服务端。
func newUploadHTTP(t *testing.T, repoDir, user, token string) *httptest.Server {
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
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write(forwardUpload(t, repoDir, "info/refs", r))
	}))
	mux.HandleFunc("/repo.git/git-upload-pack", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_, _ = w.Write(forwardUpload(t, repoDir, "git-upload-pack", r))
	}))
	return httptest.NewServer(mux)
}

func forwardUpload(t *testing.T, repoDir, action string, r *http.Request) []byte {
	t.Helper()
	args := []string{"upload-pack", "--stateless-rpc"}
	if action == "info/refs" {
		args = append(args, "--advertise-refs")
	}
	args = append(args, repoDir)
	cmd := exec.Command("git", args...)
	cmd.Env = gitEnv()
	if v := r.Header.Get("Git-Protocol"); v != "" {
		cmd.Env = append(cmd.Env, "GIT_PROTOCOL="+v)
	}
	cmd.Stdin = r.Body
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	if action == "info/refs" {
		return append([]byte("001e# service=git-upload-pack\n0000"), out...)
	}
	return out
}

// 取回范围收窄之后，旧的跟踪引用必须被剪掉。
//
// 为什么这条必须单独钉住（2026-11-24 3060 链路）：范围收窄后，上一次按旧
// 范围取回的 refs/remotes/<rm>/* 还留着，而 mergeRemoteRefs 只看
// refs/remotes/<rm>/ —— 于是「已经不在配置里」的分支每轮仍然被合并进本地，
// 冲突也永远解不掉（现场就是 meta.index 一条 bge-m3.json 冲突挂在那里）。
//
// 两阶段：先按宽范围取一次（两条都落地），再把配置收窄成一条重新取，
// 第二条的跟踪引用必须消失。
func TestFetchPrunesTrackingRefsOutsideScope(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dataDir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remoteDir)

	work := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "init", work)
	runGit(t, work, "checkout", "-b", "main.record")
	if err := os.WriteFile(filepath.Join(work, "a.md"), []byte("甲\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "a")
	runGit(t, work, "push", remoteDir, "main.record")

	runGit(t, work, "checkout", "-b", "meta.index")
	if err := os.WriteFile(filepath.Join(work, "b.md"), []byte("乙\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "b")
	runGit(t, work, "push", remoteDir, "meta.index")

	hs := newUploadHTTP(t, remoteDir, "deploy", "ghp_token_value")
	defer hs.Close()

	localDir := filepath.Join(dataDir, "repos", "daily.git")
	if _, err := gitstore.Init(localDir); err != nil {
		t.Fatal(err)
	}

	// 起一个服务端，按给定的 push 列表跑一次 fetch
	fetchWith := func(branches []string) {
		t.Helper()
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
push       = [%s]
http_user  = "deploy"
http_token = "gh"
`, dataDir, hs.URL+"/repo.git", quoteList(branches))
		if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
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
		// token 落在 dataDir 下，第二阶段已经存在，不能再 put（写入即拒绝轮换）。
		if _, err := os.Stat(filepath.Join(dataDir, "system", "secrets", "gh")); err != nil {
			if _, perr := callMCPWithVars(t, srv, rootTok, "remote-secret put gh -- $TOKEN", map[string]string{"TOKEN": "ghp_token_value"}); perr != nil {
				t.Fatalf("remote-secret put 失败: %v", perr)
			}
		}
		if _, err := callMCP(t, srv, rootTok, "fetch daily github"); err != nil {
			t.Fatalf("fetch 失败: %v", err)
		}
	}

	// 第一阶段：两条都要
	fetchWith([]string{"main.record", "meta.index"})
	repo1, err := gitstore.Open(localDir)
	if err != nil {
		t.Fatal(err)
	}
	if repo1.RemoteRef("github", "meta.index") == "" {
		t.Fatal("前置条件：第一阶段应留下 origin/meta.index")
	}

	// 第二阶段：收窄成只剩 main.record
	fetchWith([]string{"main.record"})
	repo2, err := gitstore.Open(localDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := repo2.RemoteRef("github", "meta.index"); got != "" {
		t.Fatalf("范围收窄后 origin/meta.index 必须被剪掉，实际还留着 %q", got)
	}
	if repo2.RemoteRef("github", "main.record") == "" {
		t.Fatal("范围内的小分支不该被剪掉")
	}
}

// quoteList 把分支名列表拼成 TOML 数组字面量。
func quoteList(xs []string) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, fmt.Sprintf("%q", x))
	}
	return strings.Join(parts, ", ")
}
