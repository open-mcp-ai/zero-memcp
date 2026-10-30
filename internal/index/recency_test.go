package index

import "testing"

const day = int64(86400)

// TestRecencyBoost 同样相关的内容，较新的应当排在前面。
//
// 时间尺度取真实的「天」：差一天和差三十天，增益必须不同。
// 用 1000/2000 这种小数字测不出问题——它们在 30 天半衰里几乎相同。
func TestRecencyBoost(t *testing.T) {
	const now = int64(1793000000)

	ix := New()
	ix.AddWithTime("2026-09-28-old.md", "端口固定 1729", now-30*day)
	ix.AddWithTime("2026-10-28-new.md", "端口固定 1729 订正：只绑回环", now)

	hits := ix.Search("端口 1729", 5, 0)
	if len(hits) < 2 {
		t.Fatalf("期望至少 2 个命中，得到 %d", len(hits))
	}
	if hits[0].Addr != "2026-10-28-new.md" {
		t.Errorf("新页应当排第一，得到: %+v", hits)
	}
}

// TestRecencyDoesNotOverrideRelevance 时间增益不能把无关的新内容顶上来。
func TestRecencyDoesNotOverrideRelevance(t *testing.T) {
	const now = int64(1793000000)

	ix := New()
	ix.AddWithTime("relevant-old.md", "NapCat 部署在腾讯云，端口 3000", now-60*day)
	ix.AddWithTime("irrelevant-new.md", "今天天气不错，出去走了走", now)

	hits := ix.Search("NapCat 端口", 5, 0)
	if len(hits) == 0 {
		t.Fatal("没有命中")
	}
	if hits[0].Addr != "relevant-old.md" {
		t.Errorf("无关的新内容不该因为更新就排第一: %+v", hits)
	}
}
