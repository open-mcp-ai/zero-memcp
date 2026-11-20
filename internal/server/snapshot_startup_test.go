package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 快照热启动必须比冷建快 —— 这是快照存在的**唯一理由**。
//
// 这条不变量曾经被违反过，而且没有任何症状：实测 5,000 篇
// 冷建 6.0 秒、热启动 9.2 秒（慢 50%）。
//
// 根因是 seedDedup：热启动时 Block 来自快照、Body 为 nil，于是它把
// 最近 72 小时内的每一篇都从 Git 对象里重新解压一遍，而且**串行**做。
// 冷建反而更快，因为它扫描时正文已经在内存里（Block.Body）。
//
// 快照越做越慢是很典型的退化：每加一步「恢复后处理」，只要那一步
// 需要读盘，就会把省下来的时间又还回去。所以用断言钉住方向，
// 而不是靠人记得「别让恢复路径读盘」。
func TestSnapshotStartupIsFasterThanRebuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode")
	}
	srv, dataDir := setupMountServer(t)
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}

	const N = 2000
	body := "记忆正文，中英混排 english identifiers 1729。\n第二行。\n"
	for i := 0; i < N; i++ {
		p := fmt.Sprintf("fact/2026-11-19/%05d.md", i)
		for {
			if werr := repo.Write("fact", p, []byte(body+fmt.Sprint(i)), gitstore.ModeContent); werr == nil {
				break
			}
			if _, cerr := repo.Commit("fact", "flush"); cerr != nil {
				t.Fatalf("flush: %v", cerr)
			}
		}
	}
	if _, err := repo.Commit("fact", "seed"); err != nil {
		t.Fatal(err)
	}

	// 冷建
	t0 := time.Now()
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(t0)

	// 落快照
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// 热启（必须放掉实例锁）
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now()
	srv2, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  dataDir + "/zero-memcp.toml",
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatalf("hot start: %v", err)
	}
	t.Cleanup(func() { _ = srv2.Close() })
	hot := time.Since(t1)

	t.Logf("冷建 %v / 热启 %v（%.1fx）", cold, hot, float64(cold)/float64(hot))
	if hot >= cold {
		t.Fatalf("热启动 %v 没有比冷建 %v 更快 —— 快照失去意义。"+
			"常见原因是恢复路径上又加了一步读盘（不要串行读每一篇）", hot, cold)
	}
}
