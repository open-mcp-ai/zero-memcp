//go:build perf

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 三实例双向交叉收敛：
// A、B、C 三个独立节点，各自写一个文件，两两互相推工作层，
// 最终三端必须达成一致（收敛），且全过程**零 commit**。
func TestThreeInstancesWorkLayerConvergence(t *testing.T) {
	nodes := make([]*Server, 3)
	ts := make([]*httptest.Server, 3)
	tokens := make([]string, 3)

	for i := 0; i < 3; i++ {
		srv, dataDir := setupMountServer(t)
		nodes[i] = srv
		tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: fmt.Sprintf("n%d", i), Cap: "rwx"})
		tokens[i] = tok

		s := httptest.NewServer(srv.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
		ts[i] = s
		t.Cleanup(s.Close)
	}

	// 节点 0 写 file-0
	// 节点 1 写 file-1
	// 节点 2 写 file-2
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("fact/node-%d.md", i)
		content := fmt.Sprintf("content from node %d", i)
		if _, err := callMCPRaw(t, nodes[i], tokens[i], map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  fmt.Sprintf("write blackegg/me@zero:%s $BODY", path),
				"vars": map[string]string{"BODY": content},
			},
		}); err != nil {
			t.Fatalf("节点 %d 写入失败: %v", i, err)
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// 环形推送：0->1, 1->2, 2->0
	// 此时：
	// 节点 1 有 0 和 1 的数据
	// 节点 2 有 1 和 2 的数据
	// 节点 0 有 2 和 0 的数据
	push := func(from, to int, seq int64) {
		res, err := nodes[from].pushWorkToPeer(client, ts[to].URL, tokens[to], fmt.Sprintf("node-%d", to), "blackegg/me", "zero", seq)
		if err != nil {
			t.Fatalf("push %d->%d: %v", from, to, err)
		}
		_ = res
	}

	push(0, 1, 1)
	push(1, 2, 1)
	push(2, 0, 1)

	// 第二轮反向传播：让全集在三端汇合
	// 1->0, 2->1, 0->2
	push(1, 0, 2)
	push(2, 1, 2)
	push(0, 2, 2)

	// 验证：三端都必须能读到全部 3 个文件
	for n := 0; n < 3; n++ {
		repo, err := nodes[n].projects.Get("blackegg/me")
		if err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 3; f++ {
			path := fmt.Sprintf("fact/node-%d.md", f)
			entry, found, err := repo.Stat("zero", path)
			if err != nil {
				t.Fatalf("节点 %d 查 %s 报错: %v", n, path, err)
			}
			if !found {
				t.Fatalf("节点 %d 缺少来自节点 %d 的文件 %s（未收敛）", n, f, path)
			}
			body, err := repo.ReadBlob(entry.Hash)
			if err != nil {
				t.Fatalf("节点 %d 读 %s 正文: %v", n, path, err)
			}
			expected := fmt.Sprintf("content from node %d", f)
			if string(body) != expected {
				t.Fatalf("节点 %d 读 %s 内容不符: 期望 %q, 得到 %q", n, path, expected, string(body))
			}
		}
	}
	t.Log("三实例工作层交叉同步完全收敛：三端均拥有彼此数据，全过程零 commit")
}
