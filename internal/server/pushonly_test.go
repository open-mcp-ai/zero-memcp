package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// push_only 的远端只推不拉。
//
// 为什么这条不变量必须存在（2026-11-22 配备份节点时发现）：Pull 为空会
// 回落到 Push 列表，所以「只推不拉」在配置层无法表达。而备份节点一旦
// 被回头拉，风险不是浪费流量——是**数据丢失**：
//
//	备份某分支只落地了一半（blob 缺失留下的 Missing），
//	它播种出的权威整树（Replace）就是残缺的，
//	接收端会把「批次里没有」的路径全部墓碑。
//
// 所以这里让备份端持有一条主机没有的记忆：如果主机拉了它，
// 那条记忆就会出现在主机上。push_only 生效时它必须不出现。
func TestPushOnlyRemoteNeverPulls(t *testing.T) {
	laptop, laptopData := setupMountServer(t)
	backup, backupData := setupMountServer(t)
	tokLaptop := mintRestricted(t, laptopData, auth.Claims{Sub: "zero", Cap: "rwx"})
	tokBackup := mintRestricted(t, backupData, auth.Claims{Sub: "zero", Cap: "rwx"})

	write := func(s *Server, tok, path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, s, tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + path + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// 主机上有 laptop.md；备份上有 backup-only.md（主机不该拿到它）。
	write(laptop, tokLaptop, "laptop.md", "只属于主机")
	write(backup, tokBackup, "backup-only.md", "只属于备份")

	srvBackup := httptest.NewServer(backup.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srvBackup.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 主机把备份配成 push_only 的工作层对端。
	rm := configRemote{
		Name:     "backup-164",
		URL:      srvBackup.URL,
		Token:    tokBackup,
		Push:     []string{"zero"},
		PushOnly: true,
	}
	if err := laptop.syncOpsWithRemote(client, rm, "blackegg/me"); err != nil {
		t.Fatalf("推给备份: %v", err)
	}

	// ① 推生效：备份拿到了主机的内容。
	if out, err := callMCP(t, backup, tokBackup, "read blackegg/me@zero:laptop.md"); err != nil || out == "" {
		t.Fatalf("备份应收到主机的内容: err=%v out=%q", err, out)
	}

	// ② 拉没发生：主机不该拿到只属于备份的那条。
	if out, err := callMCP(t, laptop, tokLaptop, "read blackegg/me@zero:backup-only.md"); err == nil {
		t.Fatalf("push_only 远端不该把内容写回主机，却读到了: %q", out)
	}

	// ③ 水位证明「没拉过」：拉水位必须还停在 0。
	if got := laptop.peerSeq("blackegg/me", "zero", laptop.resolvePeer("backup-164")); got != 0 {
		t.Fatalf("push_only 不该推进拉水位，实际 %d", got)
	}
}

// 反向对照：同一个对端，去掉 push_only 之后拉取必须真的发生。
//
// 没有这一条，上面那个测试在「拉取功能整体坏掉」时也会通过——
// 一个永远不拉的实现同样满足「不该拉」。
func TestWorkPeerWithoutPushOnlyDoesPull(t *testing.T) {
	laptop, laptopData := setupMountServer(t)
	backup, backupData := setupMountServer(t)
	tokLaptop := mintRestricted(t, laptopData, auth.Claims{Sub: "zero", Cap: "rwx"})
	tokBackup := mintRestricted(t, backupData, auth.Claims{Sub: "zero", Cap: "rwx"})

	write := func(s *Server, tok, path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, s, tok, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + path + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(backup, tokBackup, "backup-only.md", "只属于备份")

	srvBackup := httptest.NewServer(backup.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srvBackup.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	rm := configRemote{
		Name:  "peer",
		URL:   srvBackup.URL,
		Token: tokBackup,
		Push:  []string{"zero"},
	}
	if err := laptop.syncOpsWithRemote(client, rm, "blackegg/me"); err != nil {
		t.Fatalf("同步: %v", err)
	}
	if out, err := callMCP(t, laptop, tokLaptop, "read blackegg/me@zero:backup-only.md"); err != nil || out == "" {
		t.Fatalf("未设 push_only 时应拉到对端内容: err=%v out=%q", err, out)
	}
}

// 配置层：push_only 与 pull / fetch_only 的矛盾必须在启动期报错，
// 而不是安静地按某一个生效。
func TestPushOnlyConfigValidation(t *testing.T) {
	base := `
[server]
data = "/tmp/x"

[[user]]
name = "zero"
root = true
`
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "与 pull 冲突",
			body: `
[[remote]]
project    = "p"
name       = "backup"
url        = "http://127.0.0.1:1729"
push       = ["main"]
pull       = ["main"]
push_only  = true
auto_sync  = true
`,
			want: "push_only = true conflicts with pull",
		},
		{
			name: "与 fetch_only 冲突",
			body: `
[[remote]]
project    = "p"
name       = "backup"
url        = "http://127.0.0.1:1729"
push       = ["main"]
push_only  = true
fetch_only = true
`,
			want: "push_only = true conflicts with fetch_only",
		},
		{
			name: "单独使用合法",
			body: `
[[remote]]
project    = "p"
name       = "backup"
url        = "http://127.0.0.1:1729"
push       = ["main"]
push_only  = true
auto_sync  = true
`,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/c.toml"
			if werr := os.WriteFile(path, []byte(base+tc.body), 0o600); werr != nil {
				t.Fatal(werr)
			}
			f, err := config.Load(path)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("应合法，却报错: %v", err)
				}
				if len(f.Remotes) != 1 || !f.Remotes[0].PushOnly {
					t.Fatalf("push_only 没被解析出来: %+v", f.Remotes)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错 %q，却通过了", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}
