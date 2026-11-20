package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 索引结果单开一棵树（meta.index）：
//  1. 主记录分支一个新 blob 都不多，历史干干净净；
//  2. 排错所需的全部信息（模型、rev、切片列表、指纹）都在这棵树里；
//  3. 异步广播事件触发，让订阅的 Agent 上下文即时知道索引已更新。
func TestIndexTreeKeepsMainBlobsClean(t *testing.T) {
	srv, _ := setupMountServer(t)
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}

	// 记下主记录分支原本的条目数
	origMainEntries, err := repo.List("zero")
	if err != nil {
		t.Fatal(err)
	}

	// 假装建了一轮向量索引
	prints := map[string]string{
		"blackegg/me@zero:thought/2026-11-19/001.md": "fingerprint-alpha",
		"blackegg/me@zero:thought/2026-11-19/002.md": "fingerprint-beta",
	}

	srv.publishIndexTree("bge-m3", "weights-202411", 1024, prints)

	// 1. 主记录分支条目数一个都没变（零污染）
	afterMainEntries, err := repo.List("zero")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterMainEntries) != len(origMainEntries) {
		t.Fatalf("主记录分支条目数被污染：原来 %d，现在 %d", len(origMainEntries), len(afterMainEntries))
	}
	for _, e := range afterMainEntries {
		if strings.Contains(e.Path, "bge-m3") || strings.Contains(e.Path, "meta.index") {
			t.Fatalf("主记录分支里出现了索引文件: %s", e.Path)
		}
	}

	// 2. 索引结果确实写进了 meta.index 分支
	indexEntries, err := repo.List(indexTreeBranch)
	if err != nil {
		t.Fatalf("读取 index tree 分支: %v", err)
	}
	if len(indexEntries) == 0 {
		t.Fatal("meta.index 分支上应当有生成的索引文件，实际为空")
	}
	t.Logf("meta.index 生成了文件: %s (hash=%s)", indexEntries[0].Path, indexEntries[0].Hash[:8])

	// 3. 读取内容验证足够排错
	body, err := repo.BlobBody(indexEntries[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	var snap indexTreeSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("解析索引结果失败: %v", err)
	}
	if snap.Model != "bge-m3" || snap.Rev != "weights-202411" || snap.Dim != 1024 {
		t.Fatalf("元数据记录不符: %+v", snap)
	}
	if len(snap.Slices) != 2 {
		t.Fatalf("切片记录数应为 2，实际 %d", len(snap.Slices))
	}
	if snap.Slices["zero:thought/2026-11-19/001.md"] != "fingerprint-alpha" {
		t.Fatalf("指纹记录错误: %v", snap.Slices)
	}
	t.Logf("足够排错的索引树内容:\n%s", string(body))
}

// 模拟异步投递到上下文：MCP resources 订阅者应该收到更新通知。
func TestIndexTreePublishesEventToContext(t *testing.T) {
	srv, _ := setupMountServer(t)

	// 开一条订阅
	// 订阅这棵索引树的 URI（与 treeURI 生成的形态一致）
	watchURI := treeURI("blackegg/me", indexTreeBranch, "")
	t.Logf("订阅 URI: %s", watchURI)
	sub := srv.resources.add(Caller{User: "agent", Root: true}, []string{watchURI})
	defer srv.resources.remove(sub)

	prints := map[string]string{
		"blackegg/me@zero:thought/2026-11-19/x.md": "fp-x",
	}
	srv.publishIndexTree("test-model", "", 512, prints)

	// 验证订阅者异步收到了事件
	select {
	case ev := <-sub.ch:
		if !strings.Contains(ev.URI, "meta.index") {
			t.Fatalf("收到的事件 URI 不对: %s", ev.URI)
		}
		if ev.Origin.Identity != "semantic:test-model" {
			t.Fatalf("事件来源应指出语义模型，实际为: %+v", ev.Origin)
		}
		t.Logf("Agent 上下文成功收到异步投递: URI=%s, Identity=%s", ev.URI, ev.Origin.Identity)
	case <-time.After(2 * time.Second):
		t.Fatal("超时未收到索引更新的异步事件通知")
	}
}

// 避免未使用的 import
var _ = gitstore.ModeContent
