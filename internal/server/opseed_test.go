package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 自动播种：升级到命令流之前已存在的内容，必须在第一轮拉取时自动完整送出。
//
// 为什么必须有它（部署到 3060 之前的关键洞察）：
// 3060 上有简这十几天写的数千条记忆，但它的 oplog 目录是空的。
// 若无播种，3060 升级后对端从位点 0 拉取只会拿到「升级之后的新写入」——
// 历史整段缺失，而且两边都不报错。
// 播种把整棵树在第一次被索取时登记成一条 opTree（批量命令），
// 与后续单条命令平滑衔接。
func TestOpLogSeedsExistingTreeOnInitialPull(t *testing.T) {
	up, upData := setupMountServer(t)
	tokUp := mintRestricted(t, upData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 在没有 oplog 的情况下，确认已有内容存在。
	// setupMountServer 初始化了 SKILL.md 与 01-shape.md。
	l, err := up.opLogs.For("blackegg/me", "zero")
	if err != nil {
		t.Fatal(err)
	}
	if l.Last() != 0 {
		t.Fatalf("测试前提：启动时该分支 oplog 应为空，实际 last=%d", l.Last())
	}

	upSrv := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer upSrv.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 下游空节点从位点 0 拉取。
	downData := t.TempDir()
	down := newServerAt(t, downData)
	tokDown := mintRestricted(t, downData, auth.Claims{Sub: "zero", Cap: "rwx"})
	t.Cleanup(func() { down.Close() })

	if _, err := down.pullOpsFromPeer(client, upSrv.URL, tokUp, "up", "blackegg/me", "zero"); err != nil {
		t.Fatalf("首次拉取: %v", err)
	}

	// 1. 上游日志必须已经被播种：位点推进到 1，且这一条是 opTree 批量命令。
	if l.Last() < 1 {
		t.Fatalf("拉取后上游日志应已播种，实际 last=%d", l.Last())
	}
	first := l.All()[0]
	if first.Kind != opTree {
		t.Fatalf("第一条命令应是批量树命令 opTree，实际 %s", first.Kind)
	}
	if len(first.Batch) < 2 {
		t.Fatalf("批量命令应包含已有的文件（>=2 项），实际 %d", len(first.Batch))
	}

	// 2. 下游节点必须已经完整读到已有内容。
	out, err := callMCP(t, down, tokDown, "read blackegg/me@zero:01-shape.md")
	if err != nil {
		t.Fatalf("下游应读到播种内容: %v", err)
	}
	if !strings.Contains(out, "形状") {
		t.Fatalf("内容不对:\n%s", out)
	}

	// 3. 上游随后有新写入时，位点必须连续推进（2, 3...），不重发整树。
	if _, err := callMCPRaw(t, up, tokUp, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:after-seed.md $BODY",
			"vars": map[string]string{"BODY": "播种之后的新写入"},
		},
	}); err != nil {
		t.Fatalf("新写入: %v", err)
	}
	if l.Last() != 2 {
		t.Fatalf("新写入后位点应是 2，实际 %d", l.Last())
	}
	second := l.All()[1]
	if second.Kind != opSet || second.Path != "after-seed.md" {
		t.Fatalf("第二条应是普通 set 命令，实际 %+v", second)
	}

	// 下游增量拉取：只拿到第二条。
	res2, err := down.pullOpsFromPeer(client, upSrv.URL, tokUp, "up", "blackegg/me", "zero")
	if err != nil {
		t.Fatalf("增量拉取: %v", err)
	}
	if res2.Applied != 1 {
		t.Fatalf("增量拉取应只应用 1 条，实际 applied=%d", res2.Applied)
	}
	fmt.Print("")
}
