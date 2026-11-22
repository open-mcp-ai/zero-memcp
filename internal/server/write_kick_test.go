package server

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 写入必须尽快流到备份端，而不是干等轮询。
//
// 改之前的真实行为：noteWriteOp 登记完命令就结束，kickSync 只在 commit
// 成功后被调。主机一旦关掉自动 commit（interval=0 count=0，agent 手动
// commit 的部署方式），推送就只剩轮询——一条刚写下的记忆可能 5 分钟
// 后才到备份端，这期间掉电就真丢了。
//
// 命令流是增量的，推送也应该是增量的：登记成功即 kick，复用 sync_loop
// 里已有的 2 秒合并窗口。
func TestWriteTriggersImmediateBackupPush(t *testing.T) {
	// 备份端：一个真实的工作层对端。
	backup, backupData := setupMountServer(t)
	tokBackup := mintRestricted(t, backupData, auth.Claims{Sub: "zero", Cap: "rwx"})
	srvBackup := httptest.NewServer(backup.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srvBackup.Close()

	// 主机：自己写配置——带 auto_sync 远端，轮询间隔 1 小时。
	// 只要能收到，就只可能是写后 kick 触发的，不可能是轮询。
	hostDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostDir, "system", "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "system", "secrets", "tok-backup"),
		[]byte(tokBackup), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`
[server]
data = %q

[[user]]
name = "zero"
root = true

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwx------"

[commit]
interval = "0s"
count    = 0

[sync]
interval = "1h"

[[remote]]
project    = "blackegg/me"
name       = "backup"
url        = %q
push       = ["zero"]
push_only  = true
auto_sync  = true
http_token = "tok-backup"
`, filepath.ToSlash(hostDir), srvBackup.URL)
	cfgPath := filepath.Join(hostDir, "zero-memcp.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	host, err := New(Config{
		DataDir:     hostDir,
		ConfigFile:  cfgPath,
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	tokHost := mintRestricted(t, hostDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 手动启动同步循环（测试里 New 不调 Start）。
	host.startSyncLoop()

	// 写一条记忆。
	if _, err := callMCPRaw(t, host, tokHost, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:just-now.md $BODY",
			"vars": map[string]string{"BODY": "刚写下的这条，必须立刻到备份"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 轮询窗口是 1 小时，所以 15 秒内到达只能来自写后 kick。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, rerr := callMCP(t, backup, tokBackup, "read blackegg/me@zero:just-now.md")
		if rerr == nil && out != "" {
			t.Logf("备份端在写入后收到了内容（未等轮询）")
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("15 秒内备份端没收到刚写入的内容：写后推送没生效")
}
