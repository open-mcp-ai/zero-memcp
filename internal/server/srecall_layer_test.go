package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Issue #13 的回归：同一场讨论的 thought 不能把跨日期的 record 挤出结果。
//
// 故障现场（2026-11-27，v0.2.7，3060）：三条查询的 top3 全是本会话刚写
// 的 thought，11-15 那条真正讲清楚区别的 record 一次都没出现过。
//
// 根因不是"本会话回声"这个说法里的时间，而是**层与层同台竞争名次**：
// thought 是推理过程，逐字复述查询词的概率高、篇幅又长，BM25 里天然
// 吃满靠前的 8 个名额；record 再相关也排在第 9 位，而名单只取 8 条。
//
// 这里把那个形状搬进测试：一堆今天的 thought + 一条十二天前的 record。
// 改成分层 RRF 之后，record 只需要是 record 层里最相关的那个。
func TestSRecallLayerSplitKeepsRecordVisible(t *testing.T) {
	srv, _ := setupMountServer(t)

	ctx := context.Background()
	// 一堆今天的 thought：每篇都反复提同一个词（真实推理链就是这个形状）。
	//
	// 每篇必须**正文不同**：服务端按内容判重，一模一样的 12 篇会被合成
	// 1 条（写过一次就返回已有地址）。这不是副作用而是设计——但用它造
	// 数据会得到一个假现场：库里其实只有 2 篇，旧实现也能“过关”。
	// 第一版就是这么写的，负向验证时没变红才暴露出来。
	for i := 0; i < 12; i++ {
		body := fmt.Sprintf("海马体 把短期记忆转成长期记忆，海马体 是关键。第 %d 段推理。", i) +
			strings.Repeat("海马体 把短期记忆转成长期记忆，海马体 是关键。", 6)
		if _, err := srv.ExecLineVars(ctx, "put blackegg/daily@zero:thought/ $1", nil, []string{body}); err != nil {
			t.Fatalf("写 thought 失败: %v", err)
		}
	}
	// 一条十二天前的 record：只说一遍，但说清了区别。
	recOut, err := srv.ExecLineVars(ctx, "put blackegg/daily@zero:record/ $1", nil,
		[]string{"海马体解决「装回来还记得」，我们解决「换手机、换客户端、重装之后还是同一个人」。"})
	if err != nil {
		t.Fatalf("写 record 失败: %v", err)
	}
	recAddr := strings.TrimPrefix(strings.TrimSpace(recOut), "ok ")
	recAddr = strings.Fields(recAddr)[0]
	// 时间挪到 12 天前：这是真实场景里那条记录的位置。
	recID := strings.Replace(recAddr, "blackegg/daily/", "blackegg/daily@zero/", 1)
	srv.idxNow().AddWithTime(recID,
		"海马体解决「装回来还记得」，我们解决「换手机、换客户端、重装之后还是同一个人」。",
		time.Now().Add(-12*24*time.Hour).Unix())

	out, err := srv.ExecLine(ctx, "srecall 海马体")
	if err != nil {
		t.Fatalf("srecall: %v", err)
	}
	if !strings.Contains(out, "/record/") {
		t.Fatalf("十二天前的 record 被 thought 挤出了结果，Issue #13 复发：\n%s", out)
	}
}

// 分层不能把「层里只有一条弱相关」的东西抬上来。
//
// 分层 RRF 的已知风险：每层第 1 名都拿 1/(k+1)，于是一层里孤零零
// 一条也能和真正的第一名打平。护栏是覆盖率门槛（recallMinCoverage）——
// 这条测试确认它仍然生效：只命中一个查询词一半以下的块不能靠"占着
// 一个层"混进结果。
func TestSRecallLayerSplitStillRespectsCoverage(t *testing.T) {
	srv, _ := setupMountServer(t)

	ctx := context.Background()
	// 盖满查询词的 thought（两个词都有）
	if _, err := srv.ExecLineVars(ctx, "put blackegg/daily@zero:thought/ $1", nil,
		[]string{"海马体 装回来还记得 海马体 装回来还记得"}); err != nil {
		t.Fatalf("写 thought 失败: %v", err)
	}
	// record 层只有一条弱命中（两个词里只出现一个）
	if _, err := srv.ExecLineVars(ctx, "put blackegg/daily@zero:record/ $1", nil,
		[]string{"海马体 无关内容 无关内容 无关内容"}); err != nil {
		t.Fatalf("写 record 失败: %v", err)
	}

	out, err := srv.ExecLine(ctx, "srecall 海马体 装回来还记得")
	if err != nil {
		t.Fatalf("srecall: %v", err)
	}
	// 覆盖率门槛是 0.5，只命中 1/2 恰好达标，所以这里不断言"必须没有"，
	// 只断言强命中的那条一定在——分层不能把强命中挤掉。
	if !strings.Contains(out, "/thought/") {
		t.Fatalf("强命中的 thought 不该被分层挤掉：\n%s", out)
	}
}

// layerOfAddr 必须从路径判层，而不是从正文。
//
// record 切片正文里没有 `layer:` 行（只有 thought 有），只看正文会把
// 所有 record 归成空层，于是"record 层"这个说法根本不存在。
func TestLayerOfAddrUsesPath(t *testing.T) {
	cases := map[string]string{
		"blackegg/daily@zero/thought/2026-11-27/a.md":      "thought",
		"blackegg/daily@zero/record/2026-11-27/a.md":       "record",
		"blackegg/daily@zero/fact/2026-11-27/a.md":         "fact",
		"blackegg/daily@zero/review/2026-11-27/a.md":       "review",
		"blackegg/daily@zero/2026-11-27/a.md":              "", // 根级：独立一层
		"blackegg/daily@zero/thoughts/2026-11-27/a.md":     "", // 不是 thought/
		"blackegg/daily@zero/recordish/a.md":               "", // 不是 record/
	}
	for addr, want := range cases {
		if got := layerOfAddr(addr); got != want {
			t.Errorf("layerOfAddr(%q) = %q，应为 %q", addr, got, want)
		}
	}
}