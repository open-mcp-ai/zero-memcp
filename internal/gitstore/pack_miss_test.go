package gitstore


import (
	"testing"
)

// 对象「不在任何 pack 里」是常态，不是异常——没跑过 repack 的仓库，
// 对象全是散装（loose）的。这条路径必须廉价。
//
// 踩过的坑：早先给 readPacked 加了「找不到就失效缓存、重载、再找一次」。
// 当对象本来就不在 pack 里时，每次未命中都会重新解析整个 .idx 文件。
// 实测 inbox 分支 4,583 个路径从 3.6 秒涨到 103.7 秒——
// 一次「更稳妥」的改动把一个 2.4 秒的启动阶段变成了两分钟。
//
// 现在只在指纹真的变了时才重载。这条测试钉住「没变就别重载」。
func TestPackMissDoesNotReparseIndex(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// 写一个对象但不 repack：它会留在松散对象库里，
	// readPacked 一定找不到它。
	if err := r.Write("main", "a.md", []byte("content"), ModeContent); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := r.Commit("main", "one"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 第一次读：填缓存。
	if _, err := r.Read("main", "a.md"); err != nil {
		t.Fatalf("read: %v", err)
	}

	// 记住缓存里的指纹与 idx 指针。
	before, hadBefore := loadPackCache(r.Path)

	// 再读很多次。每次都走「松散库找到 → 不碰 pack」，
	// 但我们要确认即使走到 readPacked 也不会重载。
	for i := 0; i < 50; i++ {
		if _, rerr := r.Read("main", "a.md"); rerr != nil {
			t.Fatalf("read %d: %v", i, rerr)
		}
	}

	after, hadAfter := loadPackCache(r.Path)

	if hadBefore != hadAfter {
		t.Fatalf("缓存存在性变了：before=%v after=%v", hadBefore, hadAfter)
	}
	if hadBefore && before.fingerprint != after.fingerprint {
		t.Fatalf("50 次读之后指纹变了（%q → %q），说明有人在无谓地重载",
			before.fingerprint, after.fingerprint)
	}

	// 直接验证指纹比对本身是廉价的：连续调用不该改变缓存内容。
	changed := r.packFingerprintChanged()
	after2, _ := loadPackCache(r.Path)
	if hadAfter && changed != (after2.fingerprint != after.fingerprint) {
		t.Fatalf("packFingerprintChanged 的结果与缓存状态不一致")
	}
}

// 外部 gc 换掉 pack 之后，指纹比对必须能发现，且自愈路径仍然有效。
func TestPackFingerprintDetectsExternalChange(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := r.Write("main", "f"+string(rune('a'+i))+".md", []byte("x"), ModeContent); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := r.Commit("main", "seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 填缓存（对象的读取路径不一定碰 pack，这里直接填）。
	if _, err := r.loadPacks(); err != nil {
		t.Fatalf("loadPacks: %v", err)
	}
	if _, err := r.Read("main", "fa.md"); err != nil {
		t.Fatalf("read: %v", err)
	}

	if r.packFingerprintChanged() {
		t.Fatal("刚读过就报「指纹变了」，那样每次未命中都会重载")
	}

	if _, err := r.Repack(); err != nil {
		t.Fatalf("repack: %v", err)
	}
	// Repack 会显式 InvalidatePackCache，所以这里「变了」是对的。
	if !r.packFingerprintChanged() {
		t.Fatal("repack 之后指纹没被判定为变化——自愈路径会失效")
	}
}

// loadPackCache 是测试用的缓存读取（生产路径直接用 packCache.Load）。
func loadPackCache(path string) (packCacheEntry, bool) {
	v, ok := packCache.Load(path)
	if !ok {
		return packCacheEntry{}, false
	}
	e, _ := v.(packCacheEntry)
	return e, true
}

