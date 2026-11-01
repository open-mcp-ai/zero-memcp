package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// grep --scope 必须真的收窄范围，且 --scope 本身不能被当成正则的一部分。
//
// 这条测试盯的是两个不同的失败模式：
//  1. 参数没被剥离 → "--scope daily" 被编进正则，结果恒为零命中，
//     而报错文本是「无命中」，看起来像内容不存在；
//  2. 参数被剥离但没生效 → 别的项目的内容照样出现，范围形同虚设。
func TestGrepScope(t *testing.T) {
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

[[project]]
name  = "notes"
owner = "zero"
mode  = "rwx------"
`
	if err := osWrite(filepath.Join(dataDir, "zero-memcp.toml"), cfg); err != nil {
		t.Fatal(err)
	}
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

	// 两个项目各写一条带唯一标记的内容。
	if _, err := callMCP(t, srv, tok, "put daily -- 端口标记 ZMPORTSCOPE"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "put notes -- 端口标记 ZMPORTSCOPE"); err != nil {
		t.Fatal(err)
	}
	// 索引是提交后重建的，先折叠脏层。
	if _, err := callMCP(t, srv, tok, "commit daily@zero"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit notes@zero"); err != nil {
		t.Fatal(err)
	}

	// 1. 不带 scope：两个项目都应命中
	out, err := callMCP(t, srv, tok, "grep ZMPORTSCOPE")
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if !strings.Contains(out, "daily/") || !strings.Contains(out, "notes/") {
		t.Fatalf("不带 scope 应命中两个项目，实际: %s", out)
	}

	// 2. 限定 daily：只应有 daily 的命中
	out, err = callMCP(t, srv, tok, "grep ZMPORTSCOPE --scope daily")
	if err != nil {
		t.Fatalf("grep --scope 失败: %v", err)
	}
	if strings.Contains(out, "notes/") {
		t.Fatalf("--scope daily 不应命中 notes，实际: %s", out)
	}
	if !strings.Contains(out, "daily/") {
		t.Fatalf("--scope daily 应命中 daily，实际: %s", out)
	}

	// 3. 限定 notes：反过来验一次，防止实现把 scope 写死
	out, err = callMCP(t, srv, tok, "grep ZMPORTSCOPE --scope notes")
	if err != nil {
		t.Fatalf("grep --scope 失败: %v", err)
	}
	if strings.Contains(out, "daily/") {
		t.Fatalf("--scope notes 不应命中 daily，实际: %s", out)
	}

	// 4. 限定到不存在的范围：应无命中，而不是全量返回
	out, err = callMCP(t, srv, tok, "grep ZMPORTSCOPE --scope daily@zero/nosuchdir")
	if err != nil {
		t.Fatalf("grep 不应报错: %v", err)
	}
	if !strings.Contains(out, "无命中") {
		t.Fatalf("范围不存在时应无命中，实际: %s", out)
	}

	// 5. --scope 不能被当成正则的一部分
	if strings.Contains(out, "--scope") {
		t.Fatalf("输出里出现了 --scope，说明参数没被剥离: %s", out)
	}
}
