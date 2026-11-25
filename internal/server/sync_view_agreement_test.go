package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// `sync` 无参视图与 `sync <project> <remote>` 对同一条分支必须说同一句话。
//
// 真实翻车现场（v0.2.2 上线后实测，三端都是 v0.2.2）：
//
//	sync BlackEggAI/zero hub-3060  →  in sync (0 op)
//	sync                           →  never synced (peer is at seq 0)
//
// 同一条分支、同一时刻、两个视图两种说法。根因两层：
//
//  1. 推方向的 syncRecord 把 PeerHead 记成 len(batch.Ops)（这一轮推了几条
//     命令），而 syncPeerView 把 PeerHead 当成「对端拥有到哪一位」——
//     量纲都不是一个。
//  2. 双向对端（peer）的状态只看 in（我拉它到哪）。对端从不主动推送时
//     in 恒为 0，于是「已经对齐」被报成「从未同步」。
//
// 这个测试把两个视图的输出抓到一起比对：撤掉任一处修复都会 FAIL。
func TestSyncViewsAgreeOnPeerBranch(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	tokSrc := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	tokDst := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	if _, err := callMCPRaw(t, src, tokSrc, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:2026-11-19/agreement.md $BODY",
			"vars": map[string]string{"BODY": "两个视图必须说同一句话"},
		},
	}); err != nil {
		t.Fatalf("写: %v", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	// 只推不拉——这是真实形状（hub-3060 是双向配置，但我们从没从它拉到
	// 任何东西，in 水位恒为 0）。必须复现这一点：如果这里也拉一轮，in
	// 就非 0，撤回 peer 判据的修复测试照样绿，白测。
	rm := configRemote{
		Name:     "hub",
		URL:      dstSrv.URL,
		Token:    tokDst,
		Push:     []string{"zero"},
		PushOnly: true,
	}
	if err := src.syncOpsWithRemote(client, rm, "blackegg/me"); err != nil {
		t.Fatalf("命令流同步: %v", err)
	}

	// 本端确实把「这一轮传了几条」记下来了——无参视图的判据要用它。
	if n, ran := src.workSync.lastOutOps("blackegg/me", "zero", src.resolvePeer("hub")); !ran {
		t.Fatal("同步跑过一轮，lastOutOps 应当有记录")
	} else {
		t.Logf("这一轮实际传了 %d 条命令", n)
	}

	// 视图二：`sync` 无参拓扑视图。
	out2, err := src.syncVerbNoArgs(Caller{User: "zero", Root: true})
	if err != nil {
		t.Fatalf("sync（无参）: %v", err)
	}
	// 不能只查 "never synced"：键统一之后水位读得到 0，而 PeerHead 已经是
	// 位点，于是同一条已对齐的分支会改报 "behind 1"——一样是错的。
	// 要钉的是「它必须说 caught up」，不是「它别说不含某个词」。
	if strings.Contains(out2.Text, "never synced") {
		t.Fatalf("无参视图把已经对齐的分支报成「从未同步」——\n"+
			"同一时刻 sync <project> <remote> 报的是 in sync。\n无参视图:\n%s", out2.Text)
	}
	if strings.Contains(out2.Text, "behind") {
		t.Fatalf("无参视图把已经对齐的分支报成落后——\n"+
			"同一时刻 sync <project> <remote> 报的是 in sync。\n无参视图:\n%s", out2.Text)
	}
	if !strings.Contains(out2.Text, "caught up") {
		t.Fatalf("无参视图应当报 caught up，实际:\n%s", out2.Text)
	}
}

// 推方向记录的 PeerHead 必须是「对端拥有到哪一位」，不能是命令条数。
//
// 这一条的破坏力在于它不会让任何测试变红：PeerHead 只是个统计字段，
// 只有在别的视图拿它当位点用时才会现形（见上一个测试）。
// 所以这里直接钉住这个不变式。
func TestPushRecordPeerHeadIsPosition(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	tokSrc := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	tokDst := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	// 写三条：让「位点」与「命令条数」明确可能是不同的数。
	for _, name := range []string{"a", "b", "c"} {
		if _, err := callMCPRaw(t, src, tokSrc, map[string]any{
			"name": "term",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:2026-11-19/" + name + ".md $BODY",
				"vars": map[string]string{"BODY": "内容 " + name},
			},
		}); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	rm := configRemote{
		Name:  "hub",
		URL:   dstSrv.URL,
		Token: tokDst,
		Push:  []string{"zero"},
	}
	if err := src.syncOpsWithRemote(client, rm, "blackegg/me"); err != nil {
		t.Fatalf("命令流同步: %v", err)
	}

	var found bool
	for _, r := range src.syncLog.All() {
		if r.Dir != "push" {
			continue
		}
		found = true
		if int64(r.PeerHead) != r.To {
			t.Fatalf("push 记录的 PeerHead=%d，位点 To=%d —— PeerHead 必须表示"+
				"「对端拥有到哪一位」，不是这一轮推了几条命令", r.PeerHead, r.To)
		}
	}
	if !found {
		t.Fatal("没有找到任何 push 记录")
	}
}

// 水位键必须用「对端自报的日志属主名」，不能用配置里的别名。
//
// 为什么这条独立成测：同一台机器换个别名之后，别名键就废了，
// 水位会从头重来（resolvePeer 的注释写着这件事）。而写入端一旦
// 用了别名、读取端用了属主名，两边就永远对不上——读取端读到 0，
// 于是「已经对齐」被报成「落后」或「从未同步」。
//
// 这个测试让「别名 ≠ 属主名」成立（learnPeerIdentity），然后检查
// pushOpsToPeer 写下的水位能被属主名读到。撤销键统一即 FAIL。
func TestPushWatermarkKeyedByOwnerName(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	tokSrc := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	tokDst := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	// 配置里的名字是 "hub"，对端自报的属主名是 "node-3060"。
	src.learnPeerIdentity("hub", "node-3060")
	if got := src.resolvePeer("hub"); got != "node-3060" {
		t.Fatalf("别名没学到: resolvePeer(hub)=%q", got)
	}

	if _, err := callMCPRaw(t, src, tokSrc, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:k.md $BODY",
			"vars": map[string]string{"BODY": "键必须用属主名"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	rm := configRemote{Name: "hub", URL: dstSrv.URL, Token: tokDst, Push: []string{"zero"}, PushOnly: true}
	if err := src.syncOpsWithRemote(client, rm, "blackegg/me"); err != nil {
		t.Fatalf("同步: %v", err)
	}

	// lastOutOps 与 out 是两个独立的键写入点（noteOut / noteOutOps），
	// 两个都必须用属主名。只查 out 会漏掉 noteOutOps 那一处。
	if _, ran := src.workSync.lastOutOps("blackegg/me", "zero", "node-3060"); !ran {
		t.Fatal("按属主名 node-3060 读不到「这一轮传了几条」——" +
			"noteOutOps 用了配置别名 hub，两个写入点的键必须同时统一")
	}
	out, _ := src.workSync.cursors("blackegg/me", "zero", "node-3060")
	if out == 0 {
		t.Fatalf("按属主名 node-3060 读不到推送水位（读到 0）——"+
			"写入端用了配置别名 hub（cursors(hub)=%d）；"+
			"同一台机器换别名后水位会从头重来",
			func() int64 { o, _ := src.workSync.cursors("blackegg/me", "zero", "hub"); return o }())
	}
}

// 配置里声明成双向对端（role=peer）的分支，peerView 也要说 caught up。
//
// 真实拓扑就是这样：hub-3060 在配置里 push+pull 都声明，走的是
// peerView 的 peer 分支，而不是上文那个由流水补出来的 downstream 分支。
// 那一条分支只看 in 水位（我拉它到哪），对端从不主动推送时恒为 0，
// 于是已经对齐的分支被报成落后。撤销 peer 判据即 FAIL。
func TestPeerViewPeerRoleReportsCaughtUp(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	tokSrc := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	tokDst := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	if _, err := callMCPRaw(t, src, tokSrc, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:p.md $BODY",
			"vars": map[string]string{"BODY": "对端角色也要说对齐"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	rm := configRemote{Name: "hub", URL: dstSrv.URL, Token: tokDst, Push: []string{"zero"}, PushOnly: true}
	if err := src.syncOpsWithRemote(client, rm, "blackegg/me"); err != nil {
		t.Fatalf("同步: %v", err)
	}

	// 直接问 peer 角色那一条路径。
	v := src.peerView("blackegg/me", "zero", "hub", "peer", time.Now())
	if v.State != "caught up" {
		t.Fatalf("role=peer 的分支状态是 %q（local=%d peer=%d behind=%d），"+
			"已经对齐的分支必须报 caught up",
			v.State, v.LocalSeq, v.PeerSeq, v.Behind)
	}
}
