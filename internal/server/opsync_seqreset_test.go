package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 提交会让命令流归零、位点从 1 重来。已经记下的同步水位因此失效。
//
// 这是 2026-11-23 归零模型落地时最危险的一处：水位还停在旧位点（比如 697），
// 新流的 last 是 0，`Since(697)` 返回空 —— 于是**再也不会推任何东西给
// 对端，而且不报错**。备份就这么静默地停在旧内容上。
//
// 钉住的方向：提交之后必须继续能同步新内容。
func TestPushContinuesAfterCommitResetsSeq(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	down, downData := newWatchTestServer(t)
	downTok := mintRestricted(t, downData, auth.Claims{Sub: "zero", Name: "down", Cap: "rwx"})

	ts := httptest.NewServer(down.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 1) 上游写、提交、再写，然后推一轮
	writeDaily(t, up, upTok, "a.md", "A")
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "first"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	writeDaily(t, up, upTok, "b.md", "B")

	if _, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero"); err != nil {
		t.Fatalf("首次推送: %v", err)
	}
	if !down.repoHas("daily", "zero", "b.md") {
		t.Fatalf("首次推送后下游应有 b.md")
	}

	// 2) 上游再提交一次：命令流归零，位点从 0 重新开始，水位作废
	writeDaily(t, up, upTok, "c.md", "C")
	upLog, err := up.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	seqBeforeCommit := upLog.Last()
	if seqBeforeCommit == 0 {
		t.Fatalf("前提：提交前应有一笔待提交命令")
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "second"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := upLog.Last(); got != 0 {
		t.Fatalf("提交后流应归零，实际 %d", got)
	}

	// 3) 再写一笔，推第二轮 —— 这一笔必须到达对端
	writeDaily(t, up, upTok, "d.md", "D")

	res, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("第二轮推送: %v", err)
	}
	if !down.repoHas("daily", "zero", "d.md") {
		t.Fatalf("提交归零之后的新写入必须继续同步；实际下游没有 d.md（applied=%d）——"+
			"旧水位 %d 对不上新流，Since() 返回空，备份会静默停摆", res.Applied, seqBeforeCommit)
	}
}
