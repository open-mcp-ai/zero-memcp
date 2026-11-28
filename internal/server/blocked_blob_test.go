package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 单个 blob 取不回来绝不许卡死整条分支。
//
// 为什么这条必须钉住（2026-11-24 群内评审第 1 条）：早先 pullOpsFromPeer
// 只要有一个 blob 404 就直接 return err，整个 batch 的所有路径全部回退，
// 水位不推进。对端若永久丢了那个对象（或者只是那个文件损坏），整条分支就
// 永久停摆；而且 status 里什么都不报。
//
// 现在的行为：好对象照常落地、坏对象留证进 blocked 并在 status 点名、
// 水位钉在缺正文那条之前（保序，下次能补）。
func TestBlockedBlobDoesNotStallBranch(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	srcSrv := httptest.NewServer(src.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srcSrv.Close()

	write := func(path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, src, srcToken, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + path + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// A 写三条记忆
	write("good1.md", "第一条好记忆")
	write("broken.md", "中间这条正文将被破坏")
	write("good2.md", "第三条好记忆")

	// 找出 broken.md 对应的 SHA，并把它从 A 的磁盘上抹掉（模拟对端对象丢失）
	repoSrc, err := src.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repoSrc.DirtyChanges("zero")
	if err != nil {
		t.Fatal(err)
	}
	var brokenSHA string
	for _, ch := range changes {
		if ch.Path == "broken.md" {
			brokenSHA = ch.SHA
			break
		}
	}
	if brokenSHA == "" {
		t.Fatal("未找到 broken.md 的改动记录")
	}
	brokenObjPath := filepath.Join(srcData, "repos", "blackegg", "me.git", "objects", brokenSHA[:2], brokenSHA[2:])
	// 先读出来留着，后面做「补齐恢复」的反向对照
	backupBody, err := os.ReadFile(brokenObjPath)
	if err != nil {
		t.Fatalf("读取待破坏对象: %v", err)
	}
	if err := os.Remove(brokenObjPath); err != nil {
		t.Fatalf("破坏对象: %v", err)
	}

	// ── B 主动向 A pull ──
	client := &http.Client{Timeout: 10 * time.Second}
	if _, err := dst.pullOpsFromPeer(client, srcSrv.URL, srcToken, "peer-src", "blackegg/me", "zero"); err != nil {
		t.Fatalf("单个 blob 缺失绝不许整批报错退出，实际返回错误: %v", err)
	}

	// ① 好路径落地，坏路径不落地（防空气引用）
	if out, rerr := callMCP(t, dst, dstToken, "read blackegg/me@zero:good1.md"); rerr != nil || !strings.Contains(out, "第一条好记忆") {
		t.Fatalf("good1 应该已落地: err=%v out=%s", rerr, out)
	}
	if out, rerr := callMCP(t, dst, dstToken, "read blackegg/me@zero:good2.md"); rerr != nil || !strings.Contains(out, "第三条好记忆") {
		t.Fatalf("good2 应该已落地: err=%v out=%s", rerr, out)
	}
	mustMiss(t, dst, dstToken, "read blackegg/me@zero:broken.md")

	// ② status 必须在最显眼处点名（失败必须看得见）
	stOut, err := callMCP(t, dst, dstToken, "status")
	if err != nil {
		t.Fatalf("status 失败: %v", err)
	}
	if !strings.Contains(stOut, "BLOCKED") || !strings.Contains(stOut, "broken.md") {
		t.Fatalf("status 必须报出缺正文留证，实际输出:\n%s", stOut)
	}

	// ③ 留证必须落盘：重启后仍然看得见，不能一次重启把故障变成隐形
	if serr := dst.saveOpSyncState(); serr != nil {
		t.Fatalf("save watermark: %v", serr)
	}
	// 在另一个实例上 loadOpSyncState 验证持久化
	dst2, dstData2 := setupMountServer(t)
	// 把 dstData 的 opsync.json 拷过去
	stateBytes, err := os.ReadFile(dst.opSyncStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst2.opSyncStatePath(), stateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dst2.loadOpSyncState(); err != nil {
		t.Fatalf("loadOpSyncState: %v", err)
	}
	dst2Token := mintRestricted(t, dstData2, auth.Claims{Sub: "zero", Cap: "rwx"})
	stOut2, _ := callMCP(t, dst2, dst2Token, "status")
	if !strings.Contains(stOut2, "BLOCKED") || !strings.Contains(stOut2, "broken.md") {
		t.Fatalf("重启后 status 仍然必须报出 BLOCKED，实际:\n%s", stOut2)
	}

	// ④ 反向对照：把 blob 写回 A，下一轮 pull 必须自动治愈、清除留证
	if err := os.WriteFile(brokenObjPath, backupBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.pullOpsFromPeer(client, srcSrv.URL, srcToken, "peer-src", "blackegg/me", "zero"); err != nil {
		t.Fatalf("补齐后 pull 失败: %v", err)
	}
	// broken.md 现在必须能读到了
	if out, rerr := callMCP(t, dst, dstToken, "read blackegg/me@zero:broken.md"); rerr != nil || !strings.Contains(out, "中间这条正文将被破坏") {
		t.Fatalf("补齐后 broken.md 应已正常落地: err=%v out=%s", rerr, out)
	}
	// status 里的 BLOCKED 必须消失
	stOutAfter, _ := callMCP(t, dst, dstToken, "status")
	if strings.Contains(stOutAfter, "BLOCKED") {
		t.Fatalf("补齐后留证必须被清掉，实际仍有:\n%s", stOutAfter)
	}
}
