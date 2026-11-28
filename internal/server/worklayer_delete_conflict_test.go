package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 当 A 在工作层删除了某条记忆，而 B 在本地对该路径有未同步的改写时：
//
// 设计上「删除优先」必须保住——离线节点不得复活已删内容（见
// TestWorkLayerDeleteDoesNotResurrect）。但删除**不许静默**吃掉
// B 的未提交内容：回执里必须出现该路径，status 必须点名，
// 并明确给出被解链的本地 SHA，让人能去对象库里捞回来。
//
// 实测故障（2026-11-21）：修复前这种并发产生的结果是 Applied++，
// 回执干干净净，B 的改动无声消失——最难查的一类故障。
func TestWorkLayerDeleteOverLocalWriteIsReportedNotSilent(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	const targetPath = "2026-11-21/important.md"

	// 两端的干净历史里都有这条记忆。
	for _, pair := range []struct {
		s   *Server
		tok string
	}{{src, srcToken}, {dst, dstToken}} {
		if _, err := callMCPRaw(t, pair.s, pair.tok, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + targetPath + " $BODY",
				"vars": map[string]string{"BODY": "初始共识内容"},
			},
		}); err != nil {
			t.Fatalf("写初始: %v", err)
		}
		if _, err := callMCP(t, pair.s, pair.tok, "commit blackegg/me@zero -m init"); err != nil {
			t.Fatalf("提交初始: %v", err)
		}
	}

	// A 未提交地删除；B 未提交地改写同一路径。
	if _, err := callMCP(t, src, srcToken, "delete blackegg/me@zero/"+targetPath); err != nil {
		t.Fatalf("A 删: %v", err)
	}
	if _, err := callMCPRaw(t, dst, dstToken, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + targetPath + " $BODY",
			"vars": map[string]string{"BODY": "B 本地的最新修改"},
		},
	}); err != nil {
		t.Fatalf("B 改: %v", err)
	}

	// 记录 B 本地那条的 SHA，稍后要在冲突里看到它。
	repoDst, _ := dst.projects.Get("blackegg/me")
	before, found, err := repoDst.Stat("zero", targetPath)
	if err != nil || !found {
		t.Fatalf("B 本地条目应存在: %v", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	res, err := src.pushWorkToPeer(client, dstSrv.URL, dstToken, "dst", "blackegg/me", "zero", 1)
	if err != nil {
		t.Fatalf("推送报错: %v", err)
	}

	// 判据一：删除仍然优先（防复活不变量不能被破坏）。
	if _, stillThere, _ := repoDst.Stat("zero", targetPath); stillThere {
		t.Fatal("删除应当优先落地；本地仍有该路径说明墓碑没生效，离线节点会借此复活内容")
	}

	// 判据二：这次丢弃必须被点名，且带着被解链的 SHA。
	if len(res.Conflicted) == 0 {
		t.Fatal("删除吃掉了本地未提交内容，回执却没有任何记录——静默丢数据")
	}
	cf := res.Conflicted[0]
	if cf.Path != targetPath {
		t.Fatalf("冲突路径不符: %+v", cf)
	}
	if cf.LocalSHA != before.Hash {
		t.Fatalf("被丢弃的本地 SHA 没记录对：want %s got %s", before.Hash, cf.LocalSHA)
	}

	// 判据三：status 必须能把这件事说出来。
	statusOut, err := callMCP(t, dst, dstToken, "status")
	if err != nil {
		t.Fatalf("查 status: %v", err)
	}
	if !strings.Contains(statusOut, targetPath) {
		t.Fatalf("status 没有点名被删除冲突的路径，人无从知晓内容被解链：\n%s", statusOut)
	}
}
