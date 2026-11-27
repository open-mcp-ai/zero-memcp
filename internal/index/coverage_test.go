package index

import "testing"

// 覆盖率不达标是**降权**，不是丢弃。
//
// 这条测试以前断言的是反面（「一个巧合的 bigram 必须被拒绝，返回 0 条」）。
// 反转的理由是实测：2026-11-27 在 3060 上，Issue #13 那条正确记录
// （11-15，正文含 海马体，不含 记忆体/咋样）覆盖率 0.40，被 0.5 的
// 门槛当场杀掉——而当天那条无关的回声 thought 同样只有 0.40，一起被杀。
// 门槛没有挡住噪声，它挡住了不如它幸运的那一条。
//
// 现在的契约：巧合仍然进结果，但分数被压低，排在真正相关的那条之后。
func TestLowCoverageIsPenalizedNotDropped(t *testing.T) {
	ix := New()
	// 真正相关：查询的词基本都在
	ix.Add("p@b/good", "端口策略与权限凭证的配置说明")
	// 巧合：只撞上 计算/算机 这类通用词
	ix.Add("p@b/coincidence", "计算机上的计算任务与算机资源调度")

	hits := ix.SearchCovered("量子计算机退相干 端口策略", 5, 0, 0.5, nil, nil)
	if len(hits) < 2 {
		t.Fatalf("低覆盖率的候选不该被丢掉，应至少保留 2 条，实际 %d 条", len(hits))
	}
	if hits[0].Addr != "p@b/good" {
		t.Fatalf("真正相关的应排第一，实际第一是 %s（分数 %v）", hits[0].Addr, hits[0].Score)
	}
	for _, h := range hits {
		if h.Addr == "p@b/coincidence" && h.Score <= 0 {
			t.Fatalf("降权不该把分数打到 0（那等于变相丢弃）: %v", h.Score)
		}
	}
}

// 门槛越高，低覆盖率候选被压得越狠——但不能压到没有。
//
// 这条替代了原来的 TestCoverageZeroDisablesFilter：threshold=0 表示
// 不做折减，threshold 越高折减越重。两者都保留候选，只是分数不同。
func TestCoverageThresholdScalesPenalty(t *testing.T) {
	ix := New()
	ix.Add("p@b/doc1", "腾讯云 Windows Server 的计算机算机资源")

	permissive := ix.SearchCovered("量子计算机退相干", 3, 0, 0, nil, nil)
	strict := ix.SearchCovered("量子计算机退相干", 3, 0, 0.9, nil, nil)
	if len(permissive) == 0 {
		t.Fatal("threshold 0 不该丢弃任何候选")
	}
	if len(strict) == 0 {
		t.Fatal("threshold 0.9 也只该降权，不该丢弃")
	}
	if strict[0].Score >= permissive[0].Score {
		t.Fatalf("更高的门槛应把分数压得更低：strict=%v permissive=%v",
			strict[0].Score, permissive[0].Score)
	}
}

// 覆盖率按查询的**唯一**词计算，重复词不该稀释或放大比值。
func TestCoverageUsesDistinctQueryTokens(t *testing.T) {
	ix := New()
	ix.Add("p@b/doc1", "端口 策略 端口 策略 端口")

	if hits := ix.SearchCovered("端口 端口 端口", 3, 0, 0.5, nil, nil); len(hits) == 0 {
		t.Fatal("重复的查询词仍应命中")
	}
}

// 中文二元切分会在词边界上产生碎片，长查询的分母因此虚高。
//
// 这是 Issue #13 的真实形状：查询「海马体 和我的记忆体比咋样」切出 10 个词，
// 其中 和我/我的/的记/体比 是碎片，几乎没有任何文档能匹配；真正含
// 海马/马体/记忆 的那条只有 0.40。它必须活下来。
func TestBigramFragmentsDoNotKillTheCorrectAnswer(t *testing.T) {
	ix := New()
	// 正确的那条：只含 海马/马体/的记/记忆 四个词
	ix.Add("p@b/answer", "海马体解决「装回来还记得」，我们解决「换手机之后还是同一个人」。")
	// 回声：今天的推理链，逐字复述查询词
	ix.Add("p@b/echo", "海马体 和我的记忆体比咋样，我在想海马体的记忆体对比该怎么讲")

	hits := ix.SearchCovered("海马体 和我的记忆体比咋样", 5, 0, 0.5, nil, nil)
	found := false
	for _, h := range hits {
		if h.Addr == "p@b/answer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("覆盖率 0.40 的正确记录被丢了，Issue #13 的机制仍在：%v", hits)
	}
}
