package gitstore

import (
	"os"
	"testing"
	"time"
)

// 手工基准：真实仓库上 List() 的耗时（受 commit 深度影响）。
func TestManualListTiming(t *testing.T) {
	path := os.Getenv("ZM_REPO")
	if path == "" {
		t.Skip("set ZM_REPO")
	}
	repo, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, br := range []string{"main", "main.record", "main.chat", "main.distill", "main.dev"} {
		t0 := time.Now()
		list, err := repo.List(br)
		t.Logf("%-14s List() %7.2fs  entries=%d err=%v", br, time.Since(t0).Seconds(), len(list), err)
	}
}
