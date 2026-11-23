package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 端到端验证「以某个提交为基准拉命令流」：
// 对端已经持有某次提交的树，它只要该提交之后的增量，不要整树重发。
func TestOpPullFromBaseCommitSendsOnlyDelta(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	writeDaily(t, up, upTok, "a.md", "A")
	writeDaily(t, up, upTok, "b.md", "B")
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	sha1, err := up.commitBranch("daily", upRepo, "zero", "snapshot 1")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 基准提交之后的新写入：只有这两篇属于增量。
	writeDaily(t, up, upTok, "c.md", "C")
	writeDaily(t, up, upTok, "d.md", "D")

	ts := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	batch, err := postOpPullFrom(client, ts.URL, upTok, "daily", "zero", "down-node", 0, sha1)
	if err != nil {
		t.Fatalf("带基准提交拉取: %v", err)
	}
	if batch.BaseCommit != sha1 {
		t.Fatalf("响应应回显基准提交 %s，实际 %q", shortSHA(sha1), batch.BaseCommit)
	}
	if len(batch.Ops) == 0 {
		t.Fatalf("基准提交之后的写入应被导出，实际 0 条")
	}
	for _, op := range batch.Ops {
		if op.Kind == opTree {
			t.Fatalf("已知基准时不该退化为整树")
		}
		if op.Path == "a.md" || op.Path == "b.md" {
			t.Fatalf("%s 属于基准提交，不该出现在增量批次里", op.Path)
		}
	}
	var sawC, sawD bool
	for _, op := range batch.Ops {
		switch op.Path {
		case "c.md":
			sawC = true
		case "d.md":
			sawD = true
		}
	}
	if !sawC || !sawD {
		t.Fatalf("增量应包含 c.md 与 d.md（c=%v d=%v）", sawC, sawD)
	}
}

// 对端给了一个本地不认识的提交：必须退化为整树，不能报错。
// 报错会让老客户端永远同步不了；整树虽然贵，但永远是正确答案。
func TestOpPullFromUnknownBaseCommitFallsBackToTree(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	writeDaily(t, up, upTok, "a.md", "A")
	writeDaily(t, up, upTok, "b.md", "B")
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "snapshot 1"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	ts := httptest.NewServer(up.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	unknown := "ffffffffffffffffffffffffffffffffffffffff"
	batch, err := postOpPullFrom(client, ts.URL, upTok, "daily", "zero", "down-node", 0, unknown)
	if err != nil {
		t.Fatalf("不认识的基准不应报错: %v", err)
	}
	if batch.BaseCommit == unknown {
		t.Fatalf("认不出的基准不该被当作已满足")
	}
	var hasTree bool
	for _, op := range batch.Ops {
		if op.Kind == opTree && op.Replace {
			hasTree = true
		}
	}
	if !hasTree {
		t.Fatalf("不认识的基准必须退化为权威整树，实际 %d 条命令", len(batch.Ops))
	}
}
