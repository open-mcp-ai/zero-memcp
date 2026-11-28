package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 工作层同步的决定性检验：**两次 commit 都不发生**，A 写完 B 就能搜到。
//
// 这是整个「工作层是主体、commit 只是历史书签」设计的验收点。
// 早先的同步必须 A 先 commit、B 再 fetch，于是「A 写的东西 B 看不见」
// 是一个真实存在过的窗口；这个测试就是钉住这个窗口已经关上。
func TestWorkLayerSyncWithoutCommit(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	// B 先跑起来（它的 mux 需要一个后端）。
	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	// A 写一条记忆，**不 commit**。
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:2026-11-19/hello.md $BODY",
			"vars": map[string]string{"BODY": "hello from A"},
		},
	}); err != nil {
		t.Fatalf("A 写: %v", err)
	}

	// 确认 A 侧确实还没有 commit：工作层里有，历史里没有。
	repoSrc, err := src.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repoSrc.DirtyChanges("zero")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("A 侧工作层应当有改动，实际为空")
	}

	// A 把工作层推给 B。
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := src.pushWorkToPeer(client, dstSrv.URL, dstToken, "dst", "blackegg/me", "zero", 1)
	if err != nil {
		t.Fatalf("pus/h work layer: %v", err)
	}
	if res.Applied == 0 {
		t.Fatalf("对端应用 0 条：%+v", res)
	}

	// B 侧：**没有任何 commit 发生**，直接读工作层就能看到内容。
	repoDst, err := dst.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	body, found, err := repoDst.Stat("zero", "2026-11-19/hello.md")
	if err != nil {
		t.Fatalf("B 读工作层: %v", err)
	}
	if !found {
		t.Fatal("B 侧工作层里找不到 2026-11-19/hello.md —— 工作层同步没落地")
	}
	if body.Hash == "" {
		t.Fatal("B 侧条目没有 blob 哈希")
	}
	// 对象也在 B 上，且内容能被读出（回读校验过）
	if !repoDst.HasObject(body.Hash) {
		t.Fatalf("B 侧缺少 blob %s", body.Hash)
	}
	if _, err := repoDst.BlobBody(body.Hash); err != nil {
		t.Fatalf("B 侧 blob 读不出: %v", err)
	}

	// 最终验证：B 侧在 0 次 commit 情况下，直接用 recall 搜这条内容，
	// 必须能命中并带出刚刚写进的内容！
	out, err := callMCP(t, dst, dstToken, "recall hello")
	if err != nil {
		t.Fatalf("B 侧 recall: %v", err)
	}
	if !strings.Contains(out, "2026-11-19/hello.md") {
		t.Fatalf("B 侧 recall 结果里必须包含刚刚同步的路径，实际输出：\n%s", out)
	}
}

// 墓碑先落地：被删的内容不会被对端「复活」。
//
// 顺序就是设计本身——先应用删除，那个 blob 从此不在终局里，
// 于是本地根本不会去请求它，也就不可能把它重新装回来。
func TestWorkLayerDeleteDoesNotResurrect(t *testing.T) {
	src, srcData := setupMountServer(t)
	dst, dstData := setupMountServer(t)
	srcToken := mintRestricted(t, srcData, auth.Claims{Sub: "zero", Cap: "rwx"})
	dstToken := mintRestricted(t, dstData, auth.Claims{Sub: "zero", Cap: "rwx"})

	dstSrv := httptest.NewServer(dst.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer dstSrv.Close()

	// A 写一条（不 commit），推给 B。
	if _, err := callMCPRaw(t, src, srcToken, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:2026-11-19/gone.md $BODY",
			"vars": map[string]string{"BODY": "will be deleted"},
		},
	}); err != nil {
		t.Fatalf("A 写: %v", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if _, err := src.pushWorkToPeer(client, dstSrv.URL, dstToken, "dst", "blackegg/me", "zero", 1); err != nil {
		t.Fatalf("首轮同步: %v", err)
	}

	repoSrc, _ := src.projects.Get("blackegg/me")
	repoDst, _ := dst.projects.Get("blackegg/me")
	if _, found, _ := repoDst.Stat("zero", "2026-11-19/gone.md"); !found {
		t.Fatal("首轮同步后 B 应当有这条")
	}

	// 现在 A 删掉（同样不 commit），再同步。
	if err := repoSrc.Delete("zero", "2026-11-19/gone.md"); err != nil {
		t.Fatalf("A 删: %v", err)
	}
	if _, err := src.pushWorkToPeer(client, dstSrv.URL, dstToken, "dst", "blackegg/me", "zero", 2); err != nil {
		t.Fatalf("二轮同步: %v", err)
	}

	if _, found, _ := repoDst.Stat("zero", "2026-11-19/gone.md"); found {
		t.Fatal("B 侧这条应当已被删除（墓碑先落地），实际还在——离线节点会就此复活它")
	}
}

// 对端不验真就不推进水位：坏 blob 必须被拒收。
func TestWorkLayerRejectsCorruptBlob(t *testing.T) {
	srv, _ := setupMountServer(t)
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	// 造一个真实 blob 拿它的 sha，然后喂错内容。
	if err := repo.Write("zero", "2026-11-19/a.md", []byte("real"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	changes, err := repo.DirtyChanges("zero")
	if err != nil {
		t.Fatal(err)
	}
	var sha string
	for _, ch := range changes {
		if strings.HasSuffix(ch.Path, "2026-11-19/a.md") {
			sha = ch.SHA
		}
	}
	if sha == "" {
		t.Fatal("没找到刚写的 blob")
	}
	// 内容与名字不符 → 必须报错，不能落盘
	if err := srv.applyWorkBlob("blackegg/me", sha, []byte("tampered")); err == nil {
		t.Fatal("内容与哈希不符时应拒绝落盘，实际接受了")
	}
	// 正确内容 → 接受
	if err := srv.applyWorkBlob("blackegg/me", sha, []byte("real")); err != nil {
		t.Fatalf("正确内容被拒: %v", err)
	}
}
