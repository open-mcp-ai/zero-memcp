package weights

import (
	"path/filepath"
	"testing"
	"time"
)

func TestWeightsOpenAndBump(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.json")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	addr := "daily/2026-10-31/120000-local.md"
	if c := st.Count(addr); c != 0 {
		t.Fatalf("初始计数应为 0，得到 %d", c)
	}

	now := time.Now().Unix()
	if c := st.Bump(addr, now); c != 1 {
		t.Fatalf("首次 Bump 应为 1，得到 %d", c)
	}
	if c := st.Bump(addr, now+10); c != 2 {
		t.Fatalf("二次 Bump 应为 2，得到 %d", c)
	}
	if c := st.Count(addr); c != 2 {
		t.Fatalf("Count 应为 2，得到 %d", c)
	}

	if err := st.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重新打开应读回持久化状态
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer st2.Close()
	if c := st2.Count(addr); c != 2 {
		t.Fatalf("持久化恢复后 Count 应为 2，得到 %d", c)
	}
}

func TestWeightsPrune(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatalf("内存 State Open: %v", err)
	}
	defer st.Close()

	now := time.Now().Unix()
	st.Bump("keep1", now)
	st.Bump("keep2", now)
	st.Bump("drop1", now)

	pruned := st.Prune(func(addr string) bool {
		return addr != "drop1"
	})
	if pruned != 1 {
		t.Fatalf("应剔除 1 项，实际剔除 %d", pruned)
	}
	if st.Count("drop1") != 0 {
		t.Fatalf("drop1 计数应为 0")
	}
	if st.Count("keep1") != 1 || st.Count("keep2") != 1 {
		t.Fatalf("保留项计数异常")
	}
}
