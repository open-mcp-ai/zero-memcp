package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 这一组测试锁的是「权限只能收窄」这条承诺的反面。
//
// 三个漏洞的共同形态是同一件事：**空集与全集的表示撞了车**。
//   - 端口挂载 ∩ 凭证挂载 = 空 → 被当成「没有挂载表」= 全命名空间
//   - 凭证 cap ∩ 挂载点 cap = 空 → 空串在 hasCap 里表示「不限制」
//   - /repos/ 请求没有端口策略 → 端口挂载对 Git 端点整体失效
//
// 三者都不是「设计取舍」，是失败方向朝着放行。所以测试断言的是
// 「必须拒绝」，不是「返回什么错误文本」。

// portCall 在一个指定端口策略下执行一次 MCP 命令。
func portCall(t *testing.T, srv *Server, blk ListenBlock, token, cmd string, vars map[string]string) (string, error) {
	t.Helper()
	args := map[string]any{"cmd": cmd}
	if vars != nil {
		args["vars"] = vars
	}
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "term", "arguments": args},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.muxFor(blk).ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		return "", fmt.Errorf("%s", strings.TrimSpace(rec.Body.String()))
	}
	return mcpText(t, rec.Body.Bytes())
}

// 端口挂载与凭证挂载没有交集时，必须看不到任何东西。
//
// 曾经的失效方式：narrowByPort 返回空切片 → NewMount 返回 nil →
// Mount.Empty() 为真 → 挂载表被判为「全命名空间」，于是凭证自己的
// 挂载限制也被一起取消，能读到第三个项目的内容。
func TestPortMountDisjointFailsClosed(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/daily", At: "/"}},
	})
	// 端口只挂 blackegg/me，与凭证的 daily 无交集。
	blk := ListenBlock{
		Addr: "127.0.0.1:0", Allow: []string{"*"},
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/"}},
	}

	for _, cmd := range []string{"read 01-shape.md", "read /x.md", "recall 形状"} {
		out, err := portCall(t, srv, blk, tok, cmd, nil)
		if err == nil {
			t.Errorf("%s 应当被拒绝（端口挂载与凭证挂载无交集），却返回 %q", cmd, out)
		}
	}
}

// 同一项目但虚拟路径不相交，也算没有交集。
func TestPortMountDisjointVirtualRoot(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/secret"}},
	})
	blk := ListenBlock{
		Addr: "127.0.0.1:0", Allow: []string{"*"},
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/public"}},
	}
	if out, err := portCall(t, srv, blk, tok, "read 01-shape.md", nil); err == nil {
		t.Errorf("虚拟路径无交集应当被拒绝，却返回 %q", out)
	}
}

// 端口挂载与凭证挂载有交集时，取较窄的那个，且仍能正常工作。
func TestPortMountIntersectionStillWorks(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*"}},
	})
	blk := ListenBlock{
		Addr: "127.0.0.1:0", Allow: []string{"*"},
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero", Cap: "r"}},
	}
	out, err := portCall(t, srv, blk, tok, "read 01-shape.md", nil)
	if err != nil {
		t.Fatalf("有交集时应可读: %v", err)
	}
	if !strings.Contains(out, "形状") {
		t.Fatalf("读到的内容不对: %q", out)
	}
	// 端口把能力收窄成 r，写必须失败。
	if _, werr := portCall(t, srv, blk, tok, "put -- $TEXT", map[string]string{"TEXT": "不该写进去"}); werr == nil {
		t.Error("端口 cap=r 时写入应当被拒绝")
	}
}

// 凭证 cap 与挂载点 cap 完全不相交时，等于没有任何能力。
//
// 曾经的失效方式：intersectCap("r","w") 返回 ""，而空串在 hasCap 里
// 表示「不额外限制」——只读凭证配上只写挂载点，反而拿到了写权限。
func TestCapIntersectionEmptyIsNotUnlimited(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	// ① 凭证级 cap=r，挂载点 cap=w
	tokA := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "r",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Cap: "w"}},
	})
	blkOpen := ListenBlock{Addr: "127.0.0.1:0", Allow: []string{"*"}}
	if out, err := portCall(t, srv, blkOpen, tokA, "put -- $TEXT", map[string]string{"TEXT": "x"}); err == nil {
		t.Errorf("r∩w 为空时写入应当被拒绝，却返回 %q", out)
	}

	// ② 凭证挂载点 cap=r，端口挂载点 cap=w
	tokB := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Cap: "r"}},
	})
	blkWrite := ListenBlock{
		Addr: "127.0.0.1:0", Allow: []string{"*"},
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Cap: "w"}},
	}
	if out, err := portCall(t, srv, blkWrite, tokB, "put -- $TEXT", map[string]string{"TEXT": "x"}); err == nil {
		t.Errorf("端口 r∩w 为空时写入应当被拒绝，却返回 %q", out)
	}

	// ③ 空集也不该意外获得规则注入能力（x 位）
	tokC := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "r",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Cap: "x"}},
	})
	if out, err := portCall(t, srv, blkOpen, tokC, "put SKILL.md -- $TEXT", map[string]string{"TEXT": "x"}); err == nil {
		t.Errorf("r∩x 为空时写规则应当被拒绝，却返回 %q", out)
	}
}

// /repos/ 这条入口也必须吃到端口策略。
//
// 曾经的失效方式：muxFor 对 /repos/ 分支直接 p.next.ServeHTTP(w, r)，
// 不带 withPortPolicy，于是端口挂载对 Git 端点整体失效——
// 一张全量凭证可以 clone 端口本想隐藏的项目。
func TestGitHTTPHonorsPortMount(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	full := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	blk := ListenBlock{
		Addr: "127.0.0.1:0", Allow: []string{"git-upload-pack"},
		Mount: []auth.MountPoint{{Project: "blackegg/daily", At: "/"}},
	}
	h := srv.muxFor(blk)

	get := func(project string) int {
		req := httptest.NewRequest(http.MethodGet,
			"/repos/"+project+".git/info/refs?service=git-upload-pack", nil)
		req.Header.Set("Authorization", "Bearer "+full)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get("blackegg/daily"); code != http.StatusOK {
		t.Errorf("端口挂载内的项目应可访问，得到 %d", code)
	}
	for _, proj := range []string{"blackegg/me", "other/secret"} {
		if code := get(proj); code == http.StatusOK {
			t.Errorf("端口挂载外的项目 %s 不该通过 Git 端点访问（得到 %d）", proj, code)
		}
	}
}

// 显式 project@branch 必须写到指定的分支，不能被挂载表默认分支覆盖。
//
// 曾经的失效方式：writeBranch 在入口被设为 @branch，随后
// resolveWriteTarget 按挂载表重新推导出默认分支并覆盖它，
// 于是 put proj@chat 静默写进了别的分支，返回的 ok 地址看起来还正常。
func TestPutExplicitBranchIsHonored(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*"}},
	})

	if _, err := callMCP(t, srv, tok, "put blackegg/me@chat -- 该进 chat"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if got := repo.Pending("chat"); got != 1 {
		t.Errorf("chat 分支待提交应为 1，得到 %d（写入落到了别的分支）", got)
	}
	if got := repo.Pending("zero"); got != 0 {
		t.Errorf("zero 分支不该被这条写入碰到，待提交 %d", got)
	}
}

// 挂载点分支写 "*" 时，隐式写入必须落进默认分支，
// 不能创建一个字面叫 "*" 的分支。
func TestWildcardMountDoesNotCreateStarBranch(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub: "zero", Cap: "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "*", Cap: "rw"}},
	})
	if _, err := callMCP(t, srv, tok, "put -- 通配挂载的落点"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	for _, br := range repo.Branches() {
		if br == "*" {
			t.Fatal("创建了名为 \"*\" 的分支：通配被当成了真实分支名")
		}
	}
}

// 写入时更新的索引必须与重启后重建的索引口径一致。
//
// 曾经的失效方式：写入侧只索引正文（text），重建侧索引整份文件
// （含 frontmatter）。于是 source/endpoint/session 这些词在重启前
// 搜不到、重启后突然出现，docLen 与 BM25 分数也跟着漂。
func TestIndexParityBetweenWriteAndRebuild(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "put -- 正文标记ZQX"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 这些词只出现在 frontmatter 里，正文没有。
	probes := []string{"正文标记ZQX", "endpoint", "source", "created"}
	live := map[string]string{}
	for _, q := range probes {
		out, err := callMCP(t, srv, tok, "recall "+q)
		if err != nil {
			t.Fatalf("recall %q: %v", q, err)
		}
		live[q] = out
	}

	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}
	for _, q := range probes {
		out, err := callMCP(t, srv, tok, "recall "+q)
		if err != nil {
			t.Fatalf("reload 后 recall %q: %v", q, err)
		}
		// 只比「有没有命中」，不比分数：分数含时间衰减，会随秒数变化。
		liveHit := !strings.Contains(live[q], "no matches")
		rebuildHit := !strings.Contains(out, "no matches")
		if liveHit != rebuildHit {
			t.Errorf("查询 %q 的可检索性随重启变化：写入后命中=%v，重建后命中=%v\n写入后: %s\n重建后: %s",
				q, liveHit, rebuildHit, live[q], out)
		}
	}
}
