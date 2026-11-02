package server

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// recall 与 Reload 并发时不能竞态。
//
// execTerm 的 recall 分支在 s.mu.RUnlock() 之后调用 s.snippet，
// 而 snippet 直接读 s.trees —— 那是 Reload 在 s.mu.Lock() 里整体替换的字段。
// 读一把不在锁里的切片，等于和写方赛跑。
func TestRecallConcurrentWithReload(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	if _, err := callMCP(t, srv, tok, "put -- 形状与并发"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := callMCP(t, srv, tok, "recall 形状"); err != nil && !strings.Contains(err.Error(), "无命中") {
				t.Errorf("recall 出错: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := srv.Reload(); err != nil {
				t.Errorf("Reload 出错: %v", err)
				return
			}
		}
	}()
	time.Sleep(800 * time.Millisecond)
	close(stop)
	wg.Wait()
}
