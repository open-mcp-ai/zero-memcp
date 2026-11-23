package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 归零之后的流里只有增量命令，基准树里的内容一条命令都不带。
//
// 这是 2026-11-23 实测抓到的真故障（rock64 备份只到了 14%）：
//
//	上游写完 a.md → 提交（流归零，base = 含 a.md 的树，ops 空）
//	上游再写 b.md  → 流里只有 b.md 这一条
//	空对端来同步   → 只收到 b.md，a.md 永远不出现
//
// 根因：没有人负责把「基准树的内容」送给对端。播种只在空基准时做，
// 而归零后的基准是非空的——于是那段内容既不在任何命令里，也从不被播种。
//
// 钉住的方向：对端最终必须拿到基准确认过的全部内容，一个不少。
func TestResetStreamStillDeliversBaseContent(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	down, downData := newWatchTestServer(t)
	downTok := mintRestricted(t, downData, auth.Claims{Sub: "zero", Name: "down", Cap: "rwx"})

	ts := httptest.NewServer(down.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 1) 先写 a.md 并提交：这一步让流归零，a.md 只存在于基准树里
	writeDaily(t, up, upTok, "a.md", "A")
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "first"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	upLog, err := up.opLogs.For("daily", "zero")
	if err != nil {
		t.Fatal(err)
	}
	if upLog.BaseTree() == "" {
		t.Fatal("前提：提交后基准应指向那棵树")
	}

	// 2) 再写 b.md：此时流里只有这一条命令
	writeDaily(t, up, upTok, "b.md", "B")

	// 3) 空对端来同步：a.md 不在任何命令里，只能靠「基准内容」这条路径送达
	if _, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero"); err != nil {
		t.Fatalf("pushOpsToPeer: %v", err)
	}

	if !down.repoHas("daily", "zero", "b.md") {
		t.Fatalf("增量内容都没到（b.md）")
	}
	if !down.repoHas("daily", "zero", "a.md") {
		t.Fatalf("a.md 没到：它在基准树里、不在命令流里，而没有人负责把基准内容送出去。" +
			"结果就是备份是一棵残树——实测中 rock64 因此只拿到 14%% 的文件")
	}
}
