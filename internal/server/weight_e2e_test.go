package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// TestReadReinforcesRecall 验证闭环：read 过的东西，下次 recall 排得更前。
//
// 这是「越常用的记忆越牢固」的最小实现。要证明两件事：
//  1. 行为确实发生（b.md 读三次后排在 a.md 前面）
//  2. 状态确实落盘（weights.json 里 b.md 的计数是 3）
//
// 块不来自文件系统，而是来自裸仓库的 tree entry。
func TestReadReinforcesRecall(t *testing.T) {
	dataDir := t.TempDir()
	stateFile := filepath.Join(dataDir, "weights.json")

	repo, err := gitstore.Init(filepath.Join(dataDir, "repos", "notes.git"))
	if err != nil {
		t.Fatal(err)
	}
	// 两块内容故意高度相似：让 BM25 分不清，差异只能来自阅读权重
	if err := repo.Write("zero", "a.md", []byte("关于 termcp 的端口配置与使用"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "b.md", []byte("关于 termcp 的端口配置与说明"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "seed"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DataDir:        dataDir,
		DefaultProfile: "zero",
		Listen:         "127.0.0.1:0",
		WeightsPath:    stateFile,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()

	var base string
	for i := 0; i < 200; i++ {
		if a := srv.getListener(); a != nil {
			base = a.Addr().String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if base == "" {
		t.Fatal("服务未启动")
	}

	// 读 notes/b.md 三次
	for i := 0; i < 3; i++ {
		res, err := srv.callTool(context.Background(), "term",
			[]byte(`{"cmd":"read notes/b.md"}`), rootCaller())
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		_ = res
	}

	out, err := srv.execTerm("recall termcp 端口配置", Viewer{}, rootCaller())
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !strings.Contains(out.Text, "b.md") {
		t.Fatalf("b.md 不在命中结果里: %s", out.Text)
	}
	ib := strings.Index(out.Text, "b.md")
	ia := strings.Index(out.Text, "a.md")
	if ia >= 0 && ib > ia {
		t.Errorf("b.md 被读过 3 次却排在 a.md 之后:\n%s", out.Text)
	}

	// 等防抖落盘，验证状态持久化
	time.Sleep(900 * time.Millisecond)
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("weights.json 没生成: %v", err)
	}
	var w struct {
		Hits map[string]int `json:"hits"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("解析 weights.json: %v", err)
	}
	if w.Hits["notes/b.md"] != 3 {
		t.Errorf("期望 notes/b.md 记 3 次，得到 %d（内容: %s）",
			w.Hits["notes/b.md"], string(raw))
	}
}
