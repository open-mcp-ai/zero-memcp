package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteNoteIdempotent(t *testing.T) {
	s, _ := newTestServer(t, `
[server]
data = "."

[[project]]
name = "notes"
owner = "alice"
mode = "0755"

[[user]]
name = "alice"
root = true
`)

	res1, err := s.ExecLineVars(context.Background(), "put --session sess-1 $1", nil, []string{"太阳从东边升起"})
	if err != nil {
		t.Fatalf("第一次写入失败: %v", err)
	}
	addr1 := strings.TrimSpace(res1)
	if !strings.HasPrefix(addr1, "ok ") {
		t.Fatalf("第一次写入返回异常: %q", addr1)
	}
	path1 := strings.TrimPrefix(addr1, "ok ")

	// 第二次写一模一样的（同 kind、同 session、同 text）
	res2, err := s.ExecLineVars(context.Background(), "put --session sess-1 $1", nil, []string{"太阳从东边升起"})
	if err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}
	addr2 := strings.TrimSpace(res2)
	path2 := strings.TrimPrefix(addr2, "ok ")

	if path1 != path2 {
		t.Fatalf("幂等失效：第二次写生成了新路径 %q，而期望返回已有路径 %q", path2, path1)
	}

	// 换个 session、同一段正文 —— 按 2026-11-23 的新语义，这仍然
	// 是**同一条记忆**，必须返回已有路径。
	//
	// 早先这里断言的是反面（「不同 session 应该产生新文件」）。
	// 那条断言在 Pi 的压缩分支机制下是灾难：压缩产生新会话文件、
	// 新文件里带着完整逐字历史，重放时每个会话都把全部历史重写一遍。
	// 实测一条链 28 个会话累计 80,290 轮回答，去重后只有 6,498 轮，
	// 重复占 91.9%。出处（哪个会话）不属于这条记忆的身份。
	res3, err := s.ExecLineVars(context.Background(), "put --session sess-2 $1", nil, []string{"太阳从东边升起"})
	if err != nil {
		t.Fatalf("第三次写入失败: %v", err)
	}
	path3 := strings.TrimPrefix(strings.TrimSpace(res3), "ok ")
	if path3 != path1 {
		t.Fatalf("同一段正文换了 session 仍是同一条记忆，应返回已有路径 %q，实际拿到 %q（会话压缩后重放历史会因此写出海量重复）",
			path1, path3)
	}
}

// 重启后判重仍然有效：指纹必须与写进 frontmatter 的内容一致。
func TestDedupSurvivesRestart(t *testing.T) {
	dataDir := t.TempDir()
	cfg := `
[server]
data = "__DATA__"

[[user]]
name = "zero"
root = true

[[project]]
name  = "notes"
owner = "zero"
mode  = "rwx------"
`
	cfg = strings.ReplaceAll(cfg, "__DATA__", filepath.ToSlash(dataDir))
	_ = osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg)

	open := func() *Server {
		srv, err := New(Config{
			DataDir:     dataDir,
			ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
			Listen:      "127.0.0.1:0",
			EnforceAuth: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return srv
	}

	srv := open()
	first, err := srv.ExecLineVars(context.Background(), "put $1", nil, []string{"重启也要记得这条"})
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	_ = srv.Close()

	srv2 := open()
	defer srv2.Close()
	second, err := srv2.ExecLineVars(context.Background(), "put $1", nil, []string{"重启也要记得这条"})
	if err != nil {
		t.Fatalf("重启后写入失败: %v", err)
	}
	if first != second {
		t.Fatalf("重启后判重失效：%q vs %q", first, second)
	}
}
