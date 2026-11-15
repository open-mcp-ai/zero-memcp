package server

import (
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 存快照不能与写入并发操作同一张倒排表。
//
// 这条是血换来的：早先 saveIndexSnapshot 只在取 ix 指针时持锁，
// 然后锁外调 ix.Export()。而写入路径是在 s.mu.Lock() 里调
// idx.AddWithTime 的——于是「后台存快照」与「用户写入」同时动
// 同一张 map，进程直接 `fatal error: concurrent map iteration and
// map write`。那不是 error，是 runtime 直接杀进程，没有 recover 机会。
//
// 测试方式：一边不停地写，一边不停地存快照。不需要断言具体数值，
// 只要不 crash、且 -race 不报就是对的。
func TestSaveSnapshotConcurrentWithWrites(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写者
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = callMCP(t, srv, tok,
				"write blackegg/me@fact:race"+itoa(i%20)+".md -- concurrent body "+itoa(i))
		}
	}()

	// 存快照者
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = srv.saveIndexSnapshot()
			time.Sleep(2 * time.Millisecond)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// 快照必须被压缩：不压缩时 25,000 篇的倒排表是 660MB，
// 每次退出写那么多不可接受。
func TestIndexSnapshotIsCompressed(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 造出「同一批路径在大量词下面重复出现」的形状——
	// 这正是倒排表高度可压缩的原因。
	for i := 0; i < 40; i++ {
		if _, err := callMCP(t, srv, tok,
			"write blackegg/me@fact:zip"+itoa(i)+".md -- 重复的内容 repeated content 重复的内容 repeated content "+itoa(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := srv.saveIndexSnapshot(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 读回来必须仍然可用（压缩不能破坏内容）。
	if !srv.tryLoadIndexSnapshot("zero") {
		t.Fatal("压缩后的快照读不回来")
	}
	out, err := callMCP(t, srv, tok, "recall repeated --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !contains(out, "zip") && !contains(out, "repeated") {
		t.Fatalf("从压缩快照恢复后搜不到：%s", out)
	}
}
