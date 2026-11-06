package weights

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreOpenBumpCount(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	addr := "BlackEggAI/weft@main.record:record/2026-11-05/x.md"
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
	if st.LastRead(addr) != now+10 {
		t.Fatalf("LastRead 应为 %d，得到 %d", now+10, st.LastRead(addr))
	}
	if err := st.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer st2.Close()
	if c := st2.Count(addr); c != 2 {
		t.Fatalf("持久化恢复后 Count 应为 2，得到 %d", c)
	}
	if st2.LastRead(addr) != now+10 {
		t.Fatalf("持久化恢复后 LastRead 应为 %d，得到 %d", now+10, st2.LastRead(addr))
	}
}

// TestShardPerProject 验证按项目分片：两个项目的键写到两个文件。
func TestShardPerProject(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	a := "BlackEggAI/zero@main.record:record/a.md"
	b := "BlackEggAI/weft@main.record:record/b.md"
	now := time.Now().Unix()
	st.Bump(a, now)
	st.Bump(a, now)
	st.Bump(b, now)
	if err := st.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got, want := st.Count(a), 2; got != want {
		t.Fatalf("zero 键计数 = %d, want %d", got, want)
	}
	if got, want := st.Count(b), 1; got != want {
		t.Fatalf("weft 键计数 = %d, want %d", got, want)
	}

	for _, p := range []string{
		filepath.Join(dir, "BlackEggAI", "zero.json"),
		filepath.Join(dir, "BlackEggAI", "weft.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("分片文件不存在 %s: %v", p, err)
		}
	}
}

// TestMergeMaxSemantics 锁定合并语义：hits 取 max，last 取 max。
//
// 这是对等同步的正确性基础。取 max 而不取 sum 的理由：
// 计数是「这份记忆被用过的强度」，不是流水；两台机器各读 20 次，
// 强度是 20 而不是 40。max 同时满足交换律与幂等，反复同步不会虚增。
func TestMergeMaxSemantics(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	addr := "BlackEggAI/zero@main.record:record/x.md"
	st.Bump(addr, 100)
	st.Bump(addr, 110)
	st.Bump(addr, 120)

	// 对端：5 次但更早
	peer := map[string]entry{addr: {Hits: 5, Last: 90}}
	changed, err := st.Merge("BlackEggAI/zero", peer)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if changed != 1 {
		t.Fatalf("应有 1 键变化，得到 %d", changed)
	}
	if got, want := st.Count(addr), 5; got != want {
		t.Fatalf("hits 应取 max=5，得到 %d", got)
	}
	if got, want := st.LastRead(addr), int64(120); got != want {
		t.Fatalf("last 应取 max=120，得到 %d", got)
	}

	// 幂等：同一份数据再合一次，零变化
	changed, err = st.Merge("BlackEggAI/zero", peer)
	if err != nil {
		t.Fatalf("二次 Merge: %v", err)
	}
	if changed != 0 {
		t.Fatalf("合并应幂等（二次 0 变化），得到 %d", changed)
	}
}

// TestBareAddressFallback 无 @ 的地址落到 _bare 分片，不丢数据。
func TestBareAddressFallback(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatalf("Open 内存: %v", err)
	}
	defer st.Close()

	now := time.Now().Unix()
	if c := st.Bump("2026-11-04/some.md", now); c != 1 {
		t.Fatalf("bare 键应可记账")
	}
	if c := st.Count("2026-11-04/some.md"); c != 1 {
		t.Fatalf("bare 键 Count = %d, want 1", c)
	}
}

func TestPrune(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	now := time.Now().Unix()
	keep1 := "BlackEggAI/zero@main.record:record/keep1.md"
	keep2 := "BlackEggAI/zero@main.record:record/keep2.md"
	drop := "BlackEggAI/zero@main.record:record/drop1.md"
	st.Bump(keep1, now)
	st.Bump(keep2, now)
	st.Bump(drop, now)

	pruned := st.Prune(func(addr string) bool { return addr != drop })
	if pruned != 1 {
		t.Fatalf("应剔除 1 项，实际剔除 %d", pruned)
	}
	if st.Count(drop) != 0 {
		t.Fatalf("drop 计数应为 0")
	}
	if st.Count(keep1) != 1 || st.Count(keep2) != 1 {
		t.Fatalf("保留项计数异常")
	}
}

// TestMigrateLegacy 锁定旧格式迁移。
//
// 关键语义：
//   - 带 @ 的键是真实记录，跨 Era 相加（改名前后是不同时段）
//   - 不带 @ 的裸键是旧版双写的副本，丢弃
//   - 旧分隔符 project@branch/path 规范化为冒号
//   - 历史前缀重定向到 BlackEggAI/*
func TestMigrateLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "weights.json")
	legacyData := []byte(`{
  "hits": {
    "zero@main.record:review/x.md": 2,
    "zero-agi@review/x.md": 4,
    "dev/zero@main.record:review/x.md": 1,
    "BlackEggAI/zero@main.record:review/x.md": 3,
    "weft@main.record/record/y.md": 5,
    "zero/review/x.md": 2,
    "zero-agi/2026-11-03/x.md": 4
  },
  "last": {
    "zero@main.record:review/x.md": 100,
    "zero-agi@review/x.md": 90,
    "dev/zero@main.record:review/x.md": 80,
    "BlackEggAI/zero@main.record:review/x.md": 120,
    "weft@main.record/record/y.md": 70
  }
}`)
	if err := os.WriteFile(legacy, legacyData, 0o644); err != nil {
		t.Fatalf("写 legacy: %v", err)
	}

	shardsDir := filepath.Join(dir, "shards")
	migrated, dropped, err := MigrateLegacy(legacy, shardsDir)
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if migrated != 5 {
		t.Errorf("应迁移 5 个带@键，得到 %d", migrated)
	}
	if dropped != 2 {
		t.Errorf("应丢弃 2 个裸键，得到 %d", dropped)
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Fatalf("旧文件应已改名退役")
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("旧文件应保留为 .migrated: %v", err)
	}

	st, err := Open(shardsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// 四个 Era 的同一记忆应累加为 2+4+1+3 = 10，last 取最大 120
	xAddr := "BlackEggAI/zero@main.record:review/x.md"
	if got := st.Count(xAddr); got != 10 {
		t.Errorf("跨 Era 应累加为 10，得到 %d", got)
	}
	if got := st.LastRead(xAddr); got != 120 {
		t.Errorf("last 应取 max=120，得到 %d", got)
	}

	// 旧斜杠语法应转为冒号
	yAddr := "BlackEggAI/weft@main.record:record/y.md"
	if got := st.Count(yAddr); got != 5 {
		t.Errorf("旧斜杠键应转冒号并保留计数 5，得到 %d", got)
	}

	// 迁移后旧文件已退役，再调不会重复累加
	if _, _, err := MigrateLegacy(legacy, shardsDir); err != nil {
		t.Fatalf("二次调用应无错（文件不存在）: %v", err)
	}
	if got := st.Count(xAddr); got != 10 {
		t.Errorf("二次迁移不应改变计数，得到 %d", got)
	}
}
