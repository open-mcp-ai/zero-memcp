package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 补齐 3 个在自动化测试里调用次数为 0 的动词：whoami / diff / help。
func TestWhoamiReportsIdentityAndPrivilege(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})
	out, err := callMCP(t, srv, rootTok, "whoami")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "User: zero") || !strings.Contains(out, "root") {
		t.Fatalf("root whoami 异常: %s", out)
	}

	scopedTok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "scoped",
		Cap:   "r",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero"}},
	})
	out, err = callMCP(t, srv, scopedTok, "whoami")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "User: zero") {
		t.Fatalf("scoped whoami 异常: %s", out)
	}
}

func TestDiffReportsAddModifyDelete(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	// 单分支 diff：列出分支内全部文件的当前状态
	out, err := callMCP(t, srv, tok, "diff blackegg/me@zero")
	if err != nil {
		t.Fatalf("diff 单分支失败: %v", err)
	}
	if !strings.Contains(out, "blackegg/me@zero") {
		t.Fatalf("diff 缺少项目分支头: %s", out)
	}

	// 建一条新分支，两边产生差异
	if _, err := callMCP(t, srv, tok, "branch blackegg/me feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "write blackegg/me@feature/new.md -- 新内容"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@feature -m add-new"); err != nil {
		t.Fatal(err)
	}

	// 跨分支 diff：A -> B
	out, err = callMCP(t, srv, tok, "diff blackegg/me@zero -> blackegg/me@feature")
	if err != nil {
		t.Fatalf("跨分支 diff 失败: %v", err)
	}
	if !strings.Contains(out, "new.md") || !strings.Contains(out, "新增") {
		t.Fatalf("跨分支 diff 应认出新增文件: %s", out)
	}
}

func TestHelpReturnsGuideForKnownVerbs(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	out, err := callMCP(t, srv, tok, "help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "put") || !strings.Contains(out, "recall") || !strings.Contains(out, "since") {
		t.Fatalf("通用 help 缺少核心动词: %s", out)
	}

	for _, v := range []string{"put", "remind", "trees", "since", "remote-key", "remote-secret", "remote", "fetch", "push", "sync"} {
		vOut, err := callMCP(t, srv, tok, "help "+v)
		if err != nil {
			t.Errorf("help %s 报错: %v", v, err)
			continue
		}
		if len(strings.TrimSpace(vOut)) < 10 {
			t.Errorf("help %s 输出太短: %q", v, vOut)
		}
	}
}

// commit -m 参数支持：不能被误当作路径。
func TestCommitCustomMessage(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "write blackegg/me@zero/msg.md -- 随便写写"); err != nil {
		t.Fatal(err)
	}
	out, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m 修复了一个特定问题")
	if err != nil {
		t.Fatalf("带 -m commit 失败: %v", err)
	}
	if !strings.Contains(out, "已提交") {
		t.Fatalf("commit 回显异常: %s", out)
	}

	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	logs, err := repo.Log("zero", 1)
	if err != nil || len(logs) == 0 {
		t.Fatalf("读取 log 失败: %v", err)
	}
	if !strings.Contains(logs[0].Message, "修复了一个特定问题") {
		t.Fatalf("commit message 未被采纳: %q", logs[0].Message)
	}
}

// chmod 把内容块提升为规则块，/identity 才会注入它。
func TestChmodPromotesContentToRule(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCPWithVars(t, srv, tok, "write blackegg/me@zero/law.md -- $B", map[string]string{"B": "不许说谎"}); err != nil {
		t.Fatal(err)
	}
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	// 写完之后是普通内容
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}
	found := false
	srv.mu.RLock()
	for _, tr := range srv.trees {
		if tr.Project != "blackegg/me" || tr.Branch != "zero" {
			continue
		}
		for _, b := range tr.Injectable() {
			if b.Path == "law.md" {
				found = true
			}
		}
	}
	srv.mu.RUnlock()
	if found {
		t.Fatal("write 写出来的块不该是可注入规则")
	}

	// 提升为规则
	if _, err := callMCP(t, srv, tok, "chmod blackegg/me@zero/law.md rule"); err != nil {
		t.Fatalf("chmod 失败: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m law"); err != nil {
		t.Fatal(err)
	}

	// 正文必须完好，且现在可注入
	body, err := repo.Read("zero", "law.md")
	if err != nil || string(body) != "不许说谎" {
		t.Fatalf("chmod 不该改动正文: %v %q", err, string(body))
	}
	srv.mu.RLock()
	found = false
	for _, tr := range srv.trees {
		if tr.Project != "blackegg/me" || tr.Branch != "zero" {
			continue
		}
		for _, b := range tr.Injectable() {
			if b.Path == "law.md" {
				found = true
			}
		}
	}
	srv.mu.RUnlock()
	if !found {
		t.Fatal("chmod rule 之后应成为可注入规则")
	}

	// 再降回 content
	if _, err := callMCP(t, srv, tok, "chmod blackegg/me@zero/law.md content"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m back"); err != nil {
		t.Fatal(err)
	}
	srv.mu.RLock()
	found = false
	for _, tr := range srv.trees {
		if tr.Project != "blackegg/me" || tr.Branch != "zero" {
			continue
		}
		for _, b := range tr.Injectable() {
			if b.Path == "law.md" {
				found = true
			}
		}
	}
	srv.mu.RUnlock()
	if found {
		t.Fatal("chmod content 之后不该再注入")
	}
}

// chmod 不存在的路径必须报错；非 root 必须被拒。
func TestChmodRejectsMissingPathAndNonRoot(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	rootTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	if _, err := callMCP(t, srv, rootTok, "chmod blackegg/me@zero/nope.md rule"); err == nil {
		t.Fatal("chmod 不存在的文件应报错")
	}

	// 非 root 用户
	cfg := `[server]
data = "__D__"
[[user]]
name = "bob"
[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwxrwxrwx"
`
	_ = cfg
	bobTok := mintRestricted(t, dataDir, auth.Claims{Sub: "bob", Name: "bob", Cap: "rwx"})
	if _, err := callMCP(t, srv, bobTok, "chmod blackegg/me@zero/law.md rule"); err == nil {
		t.Fatal("非 root 不该能改模式位")
	}
}

// write 的正文通过 $VAR 传入时必须被还原，不能把编码 marker 落盘。
func TestWriteRestoresPayloadMarker(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	body := "第一行 -- 含分隔符\n第二行 $NOTAVAR\n第三行"
	if _, err := callMCPWithVars(t, srv, tok, "write blackegg/me@zero/payload.md -- $B", map[string]string{"B": body}); err != nil {
		t.Fatal(err)
	}
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.Read("zero", "payload.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("正文被破坏:\n got=%q\nwant=%q", string(got), body)
	}
	if strings.Contains(string(got), "ZMEMARG") {
		t.Fatal("编码 marker 被落盘了")
	}
}
