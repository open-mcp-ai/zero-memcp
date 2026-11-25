package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// 监听地址是否只对本机可见。
//
// 这条判断是安全红线的地基：判错一个地址，要么把对外端口当成单机
// 放行（漏），要么把单机用户挡在门外（误伤）。
func TestIsLoopbackListen(t *testing.T) {
	loop := []string{
		"127.0.0.1:1729",
		"127.0.0.1:0",
		"localhost:1729",
		"[::1]:1729",
		"[::1]:0",
	}
	for _, a := range loop {
		if !isLoopbackListen(a) {
			t.Errorf("%q 应当判为回环", a)
		}
	}
	open := []string{
		":1729",            // 空主机名 = 所有地址
		"0.0.0.0:1729",     // 全网卡
		"[::]:1729",        // 全网卡（3060 就是这个）
		"10.10.10.12:1729", // 具体网卡
		"192.168.1.5:1729",
		"example.invalid:1729", // 解析不出 IP：无法证明只对本机可见 → 不算回环
	}
	for _, a := range open {
		if isLoopbackListen(a) {
			t.Errorf("%q 不该判为回环", a)
		}
	}
}

// 监听面与鉴权姿态的自洽性（群内评审第 5 条，主人定为安全红线）。
func TestCheckAuthPosture(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		enforce bool
		blocks  []config.ListenBlock
		wantErr string // 空 = 必须放行
	}{
		{
			name:   "单机回环 + 未强制鉴权 → 放行",
			listen: "127.0.0.1:1729",
		},
		{
			name:    "对外端口 + 未强制鉴权 → 拒绝",
			listen:  "127.0.0.1:1729",
			blocks:  []config.ListenBlock{{Addr: "[::]:1729"}},
			wantErr: "enforce_auth",
		},
		{
			name:    "对外端口 + 强制鉴权 → 放行",
			listen:  "127.0.0.1:1729",
			enforce: true,
			blocks:  []config.ListenBlock{{Addr: "[::]:1729"}},
		},
		{
			name:   "对外端口 + 端口级 require=bearer → 放行",
			listen: "127.0.0.1:1729",
			blocks: []config.ListenBlock{{Addr: "0.0.0.0:1729", Require: "bearer"}},
		},
		{
			name:    "主监听就是对外地址 + 未强制鉴权 → 拒绝",
			listen:  "10.10.10.12:1729",
			wantErr: "reachable over the network",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkAuthPosture(c.listen, c.enforce, c.blocks)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应当放行，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当拒绝启动，实际放行了")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("错误信息必须点名原因（含 %q），实际: %v", c.wantErr, err)
			}
		})
	}
}

// 端到端：真正启动一个监听对外端口的实例，必须被拒绝，且错误里点名地址。
func TestNewRefusesNetworkListenerWithoutEnforceAuth(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := filepath.Join(dataDir, "zero-memcp.toml")
	raw := `
[server]
data = ` + tomlPath(dataDir) + `

enforce_auth = false

[[listen]]
addr  = "[::]:1729"
allow = ["*"]

[[user]]
name = "zero"
root = true

[[project]]
name = "blackegg/me"
owner = "zero"
mode = "rwx------"
`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{DataDir: dataDir, ConfigFile: cfgPath})
	if err == nil {
		t.Fatal("对外监听 + 未强制鉴权必须拒绝启动")
	}
	msg := err.Error()
	if !strings.Contains(msg, "[::]:1729") {
		t.Fatalf("错误必须点名具体监听地址，实际: %v", err)
	}
	if !strings.Contains(msg, "enforce_auth") {
		t.Fatalf("错误必须给出改法（enforce_auth），实际: %v", err)
	}

	// 反向对照：把 enforce_auth 打开就应该能过这一关。
	raw2 := strings.Replace(raw, "enforce_auth = false", "enforce_auth = true", 1)
	if err := os.WriteFile(cfgPath, []byte(raw2), 0o600); err != nil {
		t.Fatal(err)
	}
	okSrv, err2 := New(Config{DataDir: dataDir, ConfigFile: cfgPath})
	if err2 != nil && strings.Contains(err2.Error(), "enforce_auth") {
		t.Fatalf("开了 enforce_auth 后不该再报鉴权姿态问题: %v", err2)
	}
	if okSrv != nil {
		// 不 Close 会让实例锁的句柄一直开着——Linux 允许 unlink 打开的文件，
		// 所以只有 windows runner 会炸：
		//   TempDir RemoveAll cleanup: unlinkat ...\system\instance.lock:
		//   The process cannot access the file because it is being used by another process
		t.Cleanup(func() { _ = okSrv.Close() })
	}
}
