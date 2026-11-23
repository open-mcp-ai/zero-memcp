package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 验证全量树快照推给空下游时，所有文件都能完整到达。
func TestFullSnapshotReachesEmptyPeer(t *testing.T) {
	up, upData := newWatchTestServer(t)
	upTok := mintRestricted(t, upData, auth.Claims{Sub: "zero", Name: "up", Cap: "rwx"})

	down, downData := newWatchTestServer(t)
	downTok := mintRestricted(t, downData, auth.Claims{Sub: "zero", Name: "down", Cap: "rwx"})

	ts := httptest.NewServer(down.muxFor(ListenBlock{Addr: "127.0.0.1:0"}))
	defer ts.Close()
	client := &http.Client{Timeout: 15 * time.Second}

	// 上游写 20 个文件并提交
	for i := 0; i < 20; i++ {
		writeDaily(t, up, upTok, fmt.Sprintf("notes/doc-%03d.md", i), fmt.Sprintf("content-%d", i))
	}
	upRepo, err := up.projects.Get("daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.commitBranch("daily", upRepo, "zero", "seed 20 docs"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 推送给下游：下游是全新空库
	res, err := up.pushOpsToPeer(client, ts.URL, downTok, "down", "daily", "zero")
	if err != nil {
		t.Fatalf("pushOpsToPeer: %v", err)
	}

	t.Logf("推送结果: applied=%d deleted=%d missing=%d rejected=%d kept=%d",
		res.Applied, res.Deleted, len(res.Missing), len(res.Rejected), len(res.Kept))

	// 检查下游是否有全部 20 个文件
	missingCount := 0
	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("notes/doc-%03d.md", i)
		if !down.repoHas("daily", "zero", p) {
			missingCount++
			t.Errorf("下游缺少 %s", p)
		}
	}
	if missingCount > 0 {
		t.Fatalf("下游共缺少 %d/20 个文件！", missingCount)
	}
}
