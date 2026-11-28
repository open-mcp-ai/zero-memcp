package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 水位必须跨重启保留。
//
// 2026-11-22 真双实例狗粮实测：水位只在内存里，进程重启后 in 归零，
// 下一轮把对端整份日志重放一遍。命令幂等（applied 0），所以数据不坏、
// 日志不报错——只是每条路径都重新 refreshPath + 广播一次。
// 分支 80 个文件时是 3 秒噪音；几千个文档时就是每次重启白烧全量索引。
func TestOpSyncWatermarksSurviveRestart(t *testing.T) {
	up, upData := setupMountServer(t)
	tokUp := mintRestricted(t, upData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 上游写三条。
	for _, p := range []string{"w1.md", "w2.md", "w3.md"} {
		if _, err := callMCPRaw(t, up, tokUp, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + p + " $BODY",
				"vars": map[string]string{"BODY": "内容 " + p},
			},
		}); err != nil {
			t.Fatalf("上游写入 %s: %v", p, err)
		}
	}
	upSrv := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer upSrv.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 下游在同一个 data 目录上先同步一轮，再关掉。
	downData := t.TempDir()
	down := newServerAt(t, downData)
	tokDown := mintRestricted(t, downData, auth.Claims{Sub: "zero", Cap: "rwx"})
	if _, err := down.pullOpsFromPeer(client, upSrv.URL, tokUp, "up", "blackegg/me", "zero"); err != nil {
		t.Fatalf("首轮拉取: %v", err)
	}
	before := down.peerSeq("blackegg/me", "zero", down.resolvePeer("up"))
	if before < 3 {
		t.Fatalf("首轮之后水位应 >=3，实际 %d", before)
	}
	if err := down.Close(); err != nil {
		t.Fatalf("关闭下游: %v", err)
	}
	if _, err := os.Stat(down.opSyncStatePath()); err != nil {
		t.Fatalf("关闭时应把水位落盘: %v", err)
	}

	// 重开同一个 data 目录：水位必须还在。
	down2 := newServerAt(t, downData)
	t.Cleanup(func() { down2.Close() })
	if got := down2.peerSeq("blackegg/me", "zero", down2.resolvePeer("up")); got != before {
		t.Fatalf("重启后水位应保持 %d，实际 %d", before, got)
	}

	// 再拉一轮：没有新命令，绝不该重放。
	res, err := down2.pullOpsFromPeer(client, upSrv.URL, tokUp, "up", "blackegg/me", "zero")
	if err != nil {
		t.Fatalf("重启后拉取: %v", err)
	}
	if res.Applied != 0 {
		t.Fatalf("重启后不该重放已同步的命令，实际 applied=%d", res.Applied)
	}
	// 内容仍然可读（不是靠重放才读得到）。
	out, err := callMCP(t, down2, tokDown, "read blackegg/me@zero:w2.md")
	if err != nil {
		t.Fatalf("重启后读取: %v", err)
	}
	if out == "" {
		t.Fatal("正文为空")
	}
}

// newServerAt 在指定 data 目录上起一个最小服务（模拟"重启"）。
func newServerAt(t *testing.T, dataDir string) *Server {
	t.Helper()
	cfg := `
[server]
data = ` + tomlPath(dataDir) + `

[[user]]
name = "zero"
root = true

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwx------"
`
	if err := osWrite(dataDir+"/zero-memcp.toml", cfg); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  dataDir + "/zero-memcp.toml",
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 新节点还没写过任何东西，仓库不存在。拉取落地需要一个真实仓库——
	// 这正是「新节点入网自动拉取全量记忆」的路径，不能靠先写一条。
	if _, err := srv.projects.Ensure("blackegg/me"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	return srv
}
