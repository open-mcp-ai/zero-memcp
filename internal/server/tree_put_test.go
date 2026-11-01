package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 测试：指定树路径写入、引用写入、以及无参数列表。
func TestPutTreePrefixAndRef(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`
	_ = osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg)
	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "tester", Cap: "rwx"})

	// 1. 指定子树路径 put
	out, err := callMCP(t, srv, tok, "put team/chat -- 这是一条发在 team/chat 子树下的正文")
	if err != nil {
		t.Fatalf("put team/chat 失败: %v", err)
	}
	if !strings.Contains(out, "ok daily/team/chat/") {
		t.Fatalf("切片应落在 daily/team/chat/ 下，实际: %s", out)
	}
	slicePath := strings.TrimPrefix(strings.SplitN(out, " @ ", 2)[0], "ok ")

	// 2. trees 命令应列出该目录
	treeOut, err := callMCP(t, srv, tok, "trees daily@zero")
	if err != nil {
		t.Fatalf("trees 失败: %v", err)
	}
	if !strings.Contains(treeOut, "team") {
		t.Fatalf("trees 输出应包含 team，实际: %s", treeOut)
	}

	// 3. 引用模式 put
	refOut, err := callMCP(t, srv, tok, "put thoughts/review --ref "+slicePath)
	if err != nil {
		t.Fatalf("put --ref 失败: %v", err)
	}
	if !strings.Contains(refOut, "ok daily/thoughts/review/") {
		t.Fatalf("引用切片应落在 daily/thoughts/review/ 下，实际: %s", refOut)
	}

	// 读出引用切片，内容必须指向原文，而不是把原文全拷一遍
	refSlice := strings.TrimPrefix(strings.SplitN(refOut, " @ ", 2)[0], "ok ")
	readOut, err := callMCP(t, srv, tok, "read "+refSlice)
	if err != nil {
		t.Fatalf("read 引用失败: %v", err)
	}
	if !strings.Contains(readOut, "ref: daily@zero/team/chat/") {
		t.Fatalf("引用切片内容应是指针，实际: %s", readOut)
	}

	// 4. 不存在的引用必须被拒绝（悬空指针保护）
	_, err = callMCP(t, srv, tok, "put thoughts --ref daily@zero/not/exist.md")
	if err == nil {
		t.Fatal("引用不存在的切片必须被拒绝")
	}
}

// 测试：since 差分增量（断线续传与上下文代入）
func TestSinceTreeDelta(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`
	_ = osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg)
	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "tester", Cap: "rwx"})

	// 第一批：写一条并提交（since 需要一个 commit 作基线）
	_, err = callMCP(t, srv, tok, "put team/chat -- 条目一")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit daily@zero"); err != nil {
		t.Fatal(err)
	}
	repo, _ := srv.projects.Get("daily")
	c1, _ := repo.Resolve("zero")

	// 自 c1 查，当前应当无变化
	out1, err := callMCP(t, srv, tok, "since daily@zero/team/chat "+c1)
	if err != nil {
		t.Fatalf("since 失败: %v", err)
	}
	if !strings.Contains(out1, "没有变化") {
		t.Fatalf("没有变化时应提示无变化，实际: %s", out1)
	}

	// 第二批：写两条
	_, err = callMCP(t, srv, tok, "put team/chat -- 条目二")
	if err != nil {
		t.Fatal(err)
	}
	_, err = callMCP(t, srv, tok, "put team/other -- 别的子树")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit daily@zero"); err != nil {
		t.Fatal(err)
	}

	// 自 c1 查 team/chat 的增量：只应拿到一条 added，且不应包含 team/other 的干扰
	out2, err := callMCP(t, srv, tok, "since daily@zero/team/chat "+c1)
	if err != nil {
		t.Fatalf("since 失败: %v", err)
	}
	if !strings.Contains(out2, "added") {
		t.Fatalf("应包含 added 变化，实际: %s", out2)
	}
	if strings.Contains(out2, "team/other") {
		t.Fatalf("不应漏入别的子树变化，实际: %s", out2)
	}
}

// put 之后不能把自己的提交当成「外部提交」而触发全量重建。
func TestPutDoesNotTriggerExternalReload(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "` + dataDir + `"

[[user]]
name = "zero"
root = true

[[project]]
name  = "daily"
owner = "zero"
mode  = "rwx------"
`
	_ = osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg)
	srv, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "tester", Cap: "rwx"})
	if _, err := callMCP(t, srv, tok, "put -- 一条记录"); err != nil {
		t.Fatal(err)
	}
	// 显式 commit 推进分支头，基线应随之更新。
	if _, err := callMCP(t, srv, tok, "commit daily@zero"); err != nil {
		t.Fatal(err)
	}

	repo, _ := srv.projects.Get("daily")
	head, _ := repo.Resolve("zero")
	key := treeHeadKey("daily", "zero")
	val, ok := srv.lastCommits.Load(key)
	if !ok {
		t.Fatalf("commit 之后基线应已更新")
	}
	if val.(string) != head {
		t.Fatalf("基线应等于当前 head：%v != %s", val, head)
	}
}
