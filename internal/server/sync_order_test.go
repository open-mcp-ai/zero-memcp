package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 这一组钉的是「实时性」的排队语义（2026-11-28）。
//
// 实测背景：一个对端有 11 条分支，其中 main.chat 一条 3.9 万条。早先是
// 「先把所有分支拉完，再推所有分支」，于是刚写下的那条轻分支要等 chat
// 拉完才轮得到推。写一条记忆到对端能读到，量出来 5.5～14.2 秒，
// 而流水里那次推送本身耗时 0 毫秒——延迟全在排队。
func TestPendingBranchesGoFirstAndOrderIsStable(t *testing.T) {
	branches := []string{"fact", "main", "main.chat", "main.record", "ops"}
	pending := map[string]bool{"main": true, "ops": true}

	got := pendingFirst(branches, func(br string) bool { return pending[br] })
	// 待推的保持原相对顺序在前，其余保持原相对顺序在后。
	want := []string{"main", "ops", "fact", "main.chat", "main.record"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pendingFirst = %v, 期望 %v", got, want)
	}

	// 稳定性：同一份输入两次结果必须一致（否则问题难复现）。
	again := pendingFirst(branches, func(br string) bool { return pending[br] })
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("两次结果不一致：%v vs %v", got, again)
	}
}

// 拉取集合与推送集合要合并成一条有序表，且不重不漏。
func TestMergedBranchOrderIsDeterministicUnion(t *testing.T) {
	pull := map[string]bool{"main.chat": true, "main": true}
	push := map[string]bool{"main": true, "main.record": true, "fact": true}

	got := mergedBranchOrder(pull, push)
	// 拉取集合按字典序在前，推送集合里多出来的按字典序补在后。
	want := []string{"main", "main.chat", "fact", "main.record"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergedBranchOrder = %v, 期望 %v", got, want)
	}

	// 两轮必须一致：分支处理顺序会决定哪条先被同步，
	// 用 map 直接遍历会让同一份配置每轮表现不同。
	if second := mergedBranchOrder(pull, push); !reflect.DeepEqual(got, second) {
		t.Fatalf("两次结果不一致：%v vs %v", got, second)
	}

	// 只拉不推、只推不拉两种形态也要正确。
	if only := mergedBranchOrder(map[string]bool{"a": true}, nil); !reflect.DeepEqual(only, []string{"a"}) {
		t.Fatalf("只拉取时 = %v, 期望 [a]", only)
	}
	if only := mergedBranchOrder(nil, map[string]bool{"b": true}); !reflect.DeepEqual(only, []string{"b"}) {
		t.Fatalf("只推送时 = %v, 期望 [b]", only)
	}
}

// 逐分支「先拉后推」的关键前提：一条分支的推送只需要**它自己**的拉取
// 结果，不需要等别的分支先跑完。
//
// 这条测试用一个假对端记录请求到达顺序，断言「有本地待推内容的分支」
// 的推送发生在「别的分支的拉取」之前。两段式的旧实现下必然失败：
// 旧顺序是先拉完所有分支，被堵的那条推送永远排在后面。
func TestSyncOpsInterleavesPullAndPushPerBranch(t *testing.T) {
	s, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	_ = tok

	const project = "blackegg/me"
	// 两条分支：heavy 安静（无本地待推），light 有本地待推。
	if _, err := s.projects.Ensure(project); err != nil {
		t.Fatal(err)
	}
	repo, err := s.projects.Get(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, br := range []string{"heavy", "light"} {
		if err := repo.Write(br, "seed.md", []byte("x"), gitstore.ModeContent); err != nil {
			t.Fatalf("write %s: %v", br, err)
		}
		if _, err := repo.Commit(br, "seed"); err != nil {
			t.Fatalf("commit %s: %v", br, err)
		}
	}
	// 只有 light 产生一条命令：它才是「有东西待推」的那条。
	if _, err := s.recordTreeOp(project, "light", opSet, "new.md", "", "100644", "sha-new"); err != nil {
		t.Fatalf("record op: %v", err)
	}

	type hit struct{ kind, branch string }
	var (
		order []hit
	)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Project string `json:"project"`
			Branch  string `json:"branch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		kind := "pull"
		if strings.HasSuffix(r.URL.Path, "/push") {
			kind = "push"
		}
		order = append(order, hit{kind, req.Branch})
		w.Header().Set("Content-Type", "application/json")
		if kind == "pull" {
			// 空批次：告诉对方「我这边没有新命令」。
			_ = json.NewEncoder(w).Encode(opBatch{Project: req.Project, Branch: req.Branch, Node: "fake"})
			return
		}
		_ = json.NewEncoder(w).Encode(opApplyResult{Applied: 1})
	}))
	defer peer.Close()

	err = s.syncOpsWithRemote(&http.Client{Timeout: 10 * time.Second}, configRemote{
		Name: "fake", URL: peer.URL, Token: "t",
		Push: []string{"heavy", "light"}, Pull: []string{"heavy", "light"},
	}, project)
	if err != nil {
		t.Fatalf("syncOpsWithRemote: %v", err)
	}

	// 找出 light 的推送位置，和 heavy 的拉取位置。
	lightPush, heavyPull := -1, -1
	for i, h := range order {
		if h.branch == "light" && h.kind == "push" && lightPush < 0 {
			lightPush = i
		}
		if h.branch == "heavy" && h.kind == "pull" && heavyPull < 0 {
			heavyPull = i
		}
	}
	if lightPush < 0 || heavyPull < 0 {
		t.Fatalf("请求记录不完整: %+v", order)
	}
	if lightPush > heavyPull {
		t.Fatalf("待推分支的推送被别的分支的拉取挡住了——\n"+
			"实测形状（2026-11-28）：写一条记忆到对端能读到要 5.5～14.2 秒，\n"+
			"而推送本身耗时 0 毫秒，延迟全在排队。请求顺序: %+v", order)
	}
	// 同一条分支仍然是「先拉后推」：推之前必须先知道对端手上是哪棵树。
	lightPull := -1
	for i, h := range order {
		if h.branch == "light" && h.kind == "pull" && lightPull < 0 {
			lightPull = i
		}
	}
	if lightPull < 0 || lightPull > lightPush {
		t.Fatalf("同一条分支必须先拉后推（推送算差分要以对端树为起点），顺序: %+v", order)
	}
}
