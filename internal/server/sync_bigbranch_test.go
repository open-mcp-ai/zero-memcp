package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 规模化复现：2 万条分支 + 对端有未提交写入（have_tree 本端不认得）。
//
// 这就是生产上那条 37 秒的形状。断言的是「发出去的是差分还是整树」，
// 而不是耗时——耗时随机器变，发多少条是事实。
func TestBigBranchSnapshotScenario(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试，-short 下跳过")
	}
	// 默认小规模：这条测试要钉的是「差分还是整树」，与条数无关。
	// 需要复现生产量级（2 万条）时用 BIGN=20000 跑。
	N := 300
	if v := os.Getenv("BIGN"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			N = n
		}
	}
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	_ = mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})
	srcSrv := httptest.NewServer(src.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer srcSrv.Close()

	repoSrc, err := src.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	for i := 0; i < N; i++ {
		if err := repoSrc.Write("zero", fmt.Sprintf("d/%05d.md", i), []byte(fmt.Sprintf("记忆 %d", i)), gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	// 必须走 commitBranch：提交与「命令流归零」是成对动作。
	// 绕过它直接 repo.Commit，src 的命令流原点会停在空树，
	// 对端来拉时按位点增量处理、回 0 条——测试会假绿（第一版就是这样）。
	if _, err := src.commitBranch("blackegg/me", repoSrc, "zero", "bulk"); err != nil {
		t.Fatal(err)
	}
	t.Logf("造 %d 条耗时 %s", N, time.Since(t0).Round(time.Millisecond))

	// dst 先写一条脏层（未提交），必须在首次拉取之前。
	repoDstEarly, err := dst.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if err := repoDstEarly.Write("zero", "local-dirty.md", []byte("本端刚写、还没推"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 120 * time.Second}

	// ① 首次拉取：dst 什么也没有 → 必然是整树快照（这条允许）。
	dstLogs := captureLogs(t, dst)
	_ = dstLogs
	t1 := time.Now()
	res1, err := dst.pullOpsFromPeer(client, srcSrv.URL, srcToken, "peer-src", "blackegg/me", "zero")
	if err != nil {
		t.Fatalf("首次拉取: %v", err)
	}
	t.Logf("首次拉取结果：applied=%d missing=%d conflicted=%d rejected=%d kept=%d",
		res1.Applied, len(res1.Missing), len(res1.Conflicted), len(res1.Rejected), len(res1.Kept))
	if l, e := dst.opLogs.For("blackegg/me", "zero"); e == nil {
		t.Logf("首次拉取后 dst.base=%s", l.BaseTree())
	}
	t.Logf("① 首次拉取（整树快照，预期）耗时 %s", time.Since(t1).Round(time.Millisecond))

	repoDst, err := dst.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}

	// ② 关键：dst 在**首次拉取之前**就有一条未提交的写入。
	//
	// 这样它的 have_tree = 对端快照 + 自己那条脏层，是一棵 src 永远
	// 没有的树——正是生产上 `weft@main.record` 每写一笔就变一次的形状。
	// 放在首次拉取之后是不行的：那时 dst 已与 src 一致，have_tree 就是
	// src 认得的那棵树，测不出问题（第一版测试就是这么假绿的）。
	_ = repoDst

	// ③ src 再提交一条 → dst 需要拉到的真实变化只有 1 条。
	if err := repoSrc.Write("zero", "new-on-src.md", []byte("上游新增"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := src.commitBranch("blackegg/me", repoSrc, "zero", "one more"); err != nil {
		t.Fatal(err)
	}

	// ④ 关键：这一趟应当是差分，而不是 2 万条整树。
	srcLogs := captureLogs(t, src)
	before := dst.flattenCalls.Load()
	t2 := time.Now()
	res, err := dst.pullOpsFromPeer(client, srcSrv.URL, srcToken, "peer-src", "blackegg/me", "zero")
	if err != nil {
		t.Fatalf("第二次拉取: %v", err)
	}
	dur := time.Since(t2)
	flattens := dst.flattenCalls.Load() - before
	t.Logf("② 对端有未提交写入时的拉取：耗时 %s，applied=%d，kept=%d，摊平 %d 次",
		dur.Round(time.Millisecond), res.Applied, len(res.Kept), flattens)

	if t.Failed() {
		t.Logf("对端日志：\n%s", srcLogs.String())
		t.Logf("本端日志：\n%s", dstLogs.String())
	}
	if flattens != 0 {
		t.Fatalf("这一趟应当走差分（不摊平），实际摊平 %d 次——说明退成了整树快照", flattens)
	}
	if res.Applied > 5 {
		t.Fatalf("真实变化只有 1 条，却落地了 %d 条——退成整树快照了", res.Applied)
	}
	if len(res.Kept) != 0 {
		t.Fatalf("本端脏层不该被算成「保留」：%d 条", len(res.Kept))
	}
}
