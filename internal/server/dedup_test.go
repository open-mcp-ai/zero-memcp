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

	res1, err := s.ExecLine(context.Background(), "put @sess-1 -- 太阳从东边升起")
	if err != nil {
		t.Fatalf("第一次写入失败: %v", err)
	}
	addr1 := strings.TrimSpace(res1)
	if !strings.HasPrefix(addr1, "ok ") {
		t.Fatalf("第一次写入返回异常: %q", addr1)
	}
	path1 := strings.TrimPrefix(addr1, "ok ")

	// 第二次写一模一样的（同 kind、同 session、同 text）
	res2, err := s.ExecLine(context.Background(), "put @sess-1 -- 太阳从东边升起")
	if err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}
	addr2 := strings.TrimSpace(res2)
	path2 := strings.TrimPrefix(addr2, "ok ")

	if path1 != path2 {
		t.Fatalf("幂等失效：第二次写生成了新路径 %q，而期望返回已有路径 %q", path2, path1)
	}

	// 换个 session 应该作为新记录
	res3, err := s.ExecLine(context.Background(), "put @sess-2 -- 太阳从东边升起")
	if err != nil {
		t.Fatalf("第三次写入失败: %v", err)
	}
	path3 := strings.TrimPrefix(strings.TrimSpace(res3), "ok ")
	if path3 == path1 {
		t.Fatalf("不同 session 应该产生新文件，却返回了相同的路径: %q", path3)
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
	cfg = strings.ReplaceAll(cfg, "__DATA__", dataDir)
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
	first, err := srv.ExecLine(context.Background(), "put -- 重启也要记得这条")
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	_ = srv.Close()

	srv2 := open()
	defer srv2.Close()
	second, err := srv2.ExecLine(context.Background(), "put -- 重启也要记得这条")
	if err != nil {
		t.Fatalf("重启后写入失败: %v", err)
	}
	if first != second {
		t.Fatalf("重启后判重失效：%q vs %q", first, second)
	}
}
