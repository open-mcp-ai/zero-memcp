package index

import "testing"

func TestTokenizeHan(t *testing.T) {
	got := Tokenize("记忆服务")
	want := []Token{"记忆", "忆服", "服务"}
	if len(got) != len(want) {
		t.Fatalf("期望 %v，得到 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个: 期望 %q，得到 %q", i, want[i], got[i])
		}
	}
}

func TestTokenizeMixed(t *testing.T) {
	got := Tokenize("NapCat 部署在 C:\\tc\\napcat")
	has := func(tok Token) bool {
		for _, g := range got {
			if g == tok {
				return true
			}
		}
		return false
	}
	for _, want := range []Token{"napcat", "部署", "tc"} {
		if !has(want) {
			t.Errorf("缺少词 %q，得到 %v", want, got)
		}
	}
}

func TestSearchRanking(t *testing.T) {
	ix := New()
	ix.Add("a.md", "NapCat 部署在腾讯云的 Windows 机器上，端口 3000 与 3001")
	ix.Add("b.md", "Windows 机器的端口是动态分配的")
	ix.Add("c.md", "投屏走 WebSocket 与 WebCodecs")

	hits := ix.Search("Windows 端口", 5, 0)
	if len(hits) == 0 {
		t.Fatal("没有命中")
	}
	// a.md 和 b.md 都提到 Windows 与端口，c.md 不该出现在前面
	if hits[0].Addr == "c.md" {
		t.Errorf("c.md 不该排第一: %+v", hits)
	}
	if len(hits[0].Matched) == 0 {
		t.Errorf("命中词为空，无法解释结果")
	}
}

func TestSearchNoMatch(t *testing.T) {
	ix := New()
	ix.Add("a.md", "完全无关的内容")
	if hits := ix.Search("量子纠缠", 5, 0); len(hits) != 0 {
		t.Errorf("期望无命中，得到 %+v", hits)
	}
}

// TestIDFFloor 高频词不该主导结果。
func TestIDFFloor(t *testing.T) {
	ix := New()
	for i := 0; i < 10; i++ {
		ix.Add(string(rune('a'+i))+".md", "黑蛋说了一些话")
	}
	ix.Add("target.md", "黑蛋说端口固定 1729")

	hits := ix.Search("黑蛋 1729", 3, 1.0)
	if len(hits) == 0 {
		t.Fatal("没有命中")
	}
	if hits[0].Addr != "target.md" {
		t.Errorf("期望 target.md 排第一（它含罕见词 1729），得到 %+v", hits)
	}
}

func TestDeterministic(t *testing.T) {
	ix := New()
	ix.Add("a.md", "同样的内容")
	ix.Add("b.md", "同样的内容")

	first := ix.Search("内容", 5, 0)
	for i := 0; i < 20; i++ {
		again := ix.Search("内容", 5, 0)
		for j := range first {
			if first[j].Addr != again[j].Addr {
				t.Fatalf("排序不稳定: %v vs %v", first, again)
			}
		}
	}
}

// Remove 必须精确撤销：同一篇的词只减它自己那一次，别篇不受影响。
func TestRemoveIsExact(t *testing.T) {
	ix := New()
	ix.Add("p@b:a.md", "alpha beta gamma")
	ix.Add("p@b:b.md", "beta delta")
	if ix.Docs() != 2 {
		t.Fatalf("docs = %d, want 2", ix.Docs())
	}

	if !ix.Remove("p@b:a.md") {
		t.Fatal("remove should报告删掉了")
	}
	if ix.Docs() != 1 {
		t.Fatalf("docs = %d, want 1 after remove", ix.Docs())
	}
	// b.md 里的 beta 必须还在
	hits := ix.Search("beta", 10, 0)
	if len(hits) != 1 || hits[0].Addr != "p@b:b.md" {
		t.Fatalf("beta should only hit b.md, got %+v", hits)
	}
	// a.md 独有的词应当彻底消失（不留下空 posting）
	if hits := ix.Search("gamma", 10, 0); len(hits) != 0 {
		t.Fatalf("gamma should be gone, got %+v", hits)
	}
	// 重复删除不报错
	if ix.Remove("p@b:a.md") {
		t.Fatal("second remove should be a no-op")
	}
}
