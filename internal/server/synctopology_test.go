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

// 节点必须知道自己的上游和下游状态。
//
// 1. 我在下游问：必须知道「上游是谁、它到哪了、我落后多少」
// 2. 我在上游问：必须知道「有哪些下游在连我、各自追到哪了、有没有掉队」
// 3. 嵌入到 mem('sync') 里，不需要翻运维日志
func TestNodeKnowsUpstreamStatus(t *testing.T) {
	up, upData := setupMountServer(t)
	tokUp := mintRestricted(t, upData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 上游写 5 条。
	for i := 1; i <= 5; i++ {
		p := fmt.Sprintf("u%d.md", i)
		if _, err := callMCPRaw(t, up, tokUp, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + p + " $BODY",
				"vars": map[string]string{"BODY": "up " + p},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	upSrv := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer upSrv.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	down, downData := setupMountServer(t)
	tokDown := mintRestricted(t, downData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 下游只拉前 3 条（故意制造落后）。
	// 通过 postOpPull 直接索取，模拟「落后 2 条」的状态。
	batch, err := postOpPull(client, upSrv.URL, tokUp, "blackegg/me", "zero", "down-node", 0)
	if err != nil {
		t.Fatal(err)
	}
	// 只应用前 3 条
	batch.Ops = batch.Ops[:3]
	batch.To = batch.Ops[2].Seq
	if _, err := down.ingestOps("blackegg/me", "zero", batch); err != nil {
		t.Fatal(err)
	}

	// 记下一轮 pull：下游知道上游当前位点是 len(all)，而自己只落到了 seq 3。
	down.recordSync(syncRecord{
		Dir: "pull", Peer: "hub-3060", Node: "up-node",
		Project: "blackegg/me", Branch: "zero",
		From: 0, To: 3, PeerHead: 5,
		Applied: 3,
	})

	out, err := callMCP(t, down, tokDown, "sync")
	if err != nil {
		t.Fatal(err)
	}

	// 下游视角判据：
	// 1. 明确标出 upstream / hub
	// 2. 明确算出落后条数（behind 2）
	// 3. 标出当前本地位点（3）与上游位点（5）
	if !strings.Contains(out, "upstream:") {
		t.Fatalf("下游视角必须有 upstream 区块:\n%s", out)
	}
	if !strings.Contains(out, "behind 2") {
		t.Fatalf("必须准确报出落后条数 (behind 2):\n%s", out)
	}
	if !strings.Contains(out, "local 3") || !strings.Contains(out, "peer 5") {
		t.Fatalf("必须报出本地位点与上游位点:\n%s", out)
	}
}

func TestNodeKnowsDownstreamStatus(t *testing.T) {
	up, upData := setupMountServer(t)
	tokUp := mintRestricted(t, upData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 上游写 10 条。
	for i := 1; i <= 10; i++ {
		p := fmt.Sprintf("msg%d.md", i)
		if _, err := callMCPRaw(t, up, tokUp, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + p + " $BODY",
				"vars": map[string]string{"BODY": "msg"},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 模拟两个不同的下游来拉过，用真实位点：
	//   laptop 追到当前头（caught up）
	//   phone  只追到 head-4（behind 4）
	head := up.localOpHead("blackegg/me", "zero")
	if head < 5 {
		t.Fatalf("测试前提：写入 10 条后本地日志头应 >=5，实际 %d", head)
	}
	up.recordSync(syncRecord{
		Dir: "served", Peer: "laptop", Node: "laptop",
		Project: "blackegg/me", Branch: "zero",
		From: 0, To: head, Applied: int(head),
	})
	up.recordSync(syncRecord{
		Dir: "served", Peer: "phone", Node: "phone",
		Project: "blackegg/me", Branch: "zero",
		From: 0, To: head - 4, Applied: int(head - 4),
	})

	out, err := callMCP(t, up, tokUp, "sync")
	if err != nil {
		t.Fatal(err)
	}

	// 上游视角判据：
	// 1. 明确标出 downstream 区块
	// 2. 列出两个不同的下游：laptop 和 phone
	// 3. 标出各自进度：laptop 追平，phone 落后 4 条
	if !strings.Contains(out, "downstreams (") {
		t.Fatalf("上游视角必须有 downstreams 区块:\n%s", out)
	}
	if !strings.Contains(out, "laptop") || !strings.Contains(out, "phone") {
		t.Fatalf("必须列出所有已知的下游节点:\n%s", out)
	}
	if !strings.Contains(out, "caught up") {
		t.Fatalf("必须标出已追平的节点:\n%s", out)
	}
	if !strings.Contains(out, "behind 4") {
		t.Fatalf("必须准确报出下游落后的条数 (behind 4):\n%s", out)
	}
}
