package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 对端基准记忆过期时，推送必须自己回落到整树快照，而不是永久卡在同一个位置。
//
// 实测来源（2026-11-25，rock64 停机 19 小时）：
//
//	rock64 的冷备份容器停了 19 小时。期间本机折过若干次 commit，命令流换了
//	原点、内容树前进了几代；而本端关于「对端手上是哪棵树」的记忆还停在旧的
//	那棵。于是发送方按旧基准算差分，接收方拿自己的基准一比对就拒收：
//
//	  op-sync BlackEggAI/weft@main.record: base tree mismatch (local 6ec22af8, peer 605e6f66)
//
//	下一轮、再下一轮，还是同一批、同一个位置。错误信息在叫**人**去要一份
//	整树快照——没有人会去，这条备份就永久停在那里。
//
// 正确行为：被这么拒一次，就等于对端在说「我不认识你那个基准」，本端应当
// 把自己的记忆作废，按「对端手上什么都没有」重新导出，导出器自然选整树快照。
func TestPushFallsBackToSnapshotOnBaseMismatch(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	const project, branch = "blackegg/me", "zero"
	write := func(path, body string) {
		t.Helper()
		if _, err := callMCPRaw(t, src, srcToken, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write " + project + "@" + branch + ":" + path + " $BODY",
				"vars": map[string]string{"BODY": body},
			},
		}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	push := func(seq int) error {
		_, err := src.pushOpsToPeer(client, dstSrv.URL, dstToken, "dst", project, branch)
		return err
	}

	// 第一代内容：写 a.md 并推给对端，两边对齐。
	write("a.md", "第一代")
	if err := push(1); err != nil {
		t.Fatalf("首次推送: %v", err)
	}

	// 记下这一代**真实的内容树**（只含 a.md），待会儿用它伪造一份过期记忆。
	// 必须是真实的非空树：空树会被导出器认成「对端什么都没有」，直接走
	// 整树快照，根本走不到差分那条会撞上基准不符的路径。
	staleTree := contentTreeOf(t, src, project, branch)
	if staleTree == "" || staleTree == gitstore.EmptyTreeSHA {
		t.Fatalf("必须拿到第一代的真实内容树，实际 %q", staleTree)
	}

	// 再来一代并推送：对端基准前进到新的树。
	write("b.md", "第二代")
	if err := push(2); err != nil {
		t.Fatalf("第二次推送: %v", err)
	}

	// 伪造「停机期间本端记错了对端基准」：把记忆退回到第一代那棵树。
	// 此时本端会按第一代的基准算差分，而接收方的基准已经是第二代——
	// 正是线上那条 base tree mismatch。
	src.workSync.notePeerTree(project, branch, src.resolvePeer("dst"), staleTree)
	t.Logf("伪造过期记忆: peerKey=%s staleTree=%s", src.resolvePeer("dst"), shortSHA(staleTree))

	// 后端又写了一条新内容，触发一次必须发出去的推送。
	write("c.md", "第三代")

	// 关键断言：这一次推送必须成功。早先它会一直返回 base tree mismatch。
	if err := push(3); err != nil {
		t.Fatalf("对端基准不一致时必须自动回落整树快照，实际报错: %v", err)
	}

	// 且内容真的到了对端（不只是「没报错」）。
	repoDst, err := dst.projects.Get(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		if _, found, _ := repoDst.Stat(branch, p); !found {
			t.Fatalf("对端应当有 %s，实际没有", p)
		}
	}
	_ = strconv.Itoa
}

// contentTreeOf 把一棵分支的当前内容算成一棵内容树（与导出器同一算法）。
//
// 测试需要「一棵真实存在、但已经不是对端基准」的树来伪造过期记忆；
// 只有真实的内容树才会让导出器选择「树到树差分」，也就才会撞上
// 接收方的基准校验——那正是线上那条 base tree mismatch 的来路。
func contentTreeOf(t *testing.T, s *Server, project, branch string) string {
	t.Helper()
	entries, err := s.flattenTreeToOps(project, branch)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		t.Fatal(err)
	}
	pathMap := make(map[string]gitstore.Entry, len(entries))
	for _, e := range entries {
		pathMap[e.Path] = gitstore.Entry{Path: e.Path, Mode: gitstore.Mode(e.Mode), Hash: e.SHA}
	}
	tree, err := repo.WriteTreeFromPaths(pathMap)
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}
	return tree
}
