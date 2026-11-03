package index

import "testing"

// A query that shares one common bigram with a document is not a match.
//
// Chinese tokenization produces bigrams, so "量子计算机退相干" yields 计算 and
// 算机 - words that appear in any document about computers. Without a coverage
// floor that query "matched" a memory about a cloud server at a high score,
// which broke the layered fallback in two ways at once: the caller saw an
// unrelated memory as an answer, and the next layer was never asked because
// the first layer had "answered".
func TestSearchRejectsSingleTokenCoincidence(t *testing.T) {
	ix := New()
	ix.Add("p@b/doc1", "腾讯云 Windows Server 的端口策略与权限凭证配置")
	// doc2 故意含「计算」「算机」：这正是「量子计算机」会撞上的通用词。
	ix.Add("p@b/doc2", "记忆库索引与分支提交的设计，计算机上的计算任务与算机资源调度")

	// Genuinely related: every query term is present.
	if hits := ix.SearchCovered("端口策略", 3, 0, 0.5, nil, nil); len(hits) == 0 {
		t.Fatal("a query whose terms all appear should match")
	}

	// Unrelated: shares only 计算/算机 with a doc that talks about computers.
	// A plain Search() returns this; a covered search must not.
	if hits := ix.Search("量子计算机退相干", 3, 0); len(hits) == 0 {
		t.Fatal("precondition: plain search is expected to be permissive here")
	}
	if hits := ix.SearchCovered("量子计算机退相干", 3, 0, 0.5, nil, nil); len(hits) != 0 {
		t.Fatalf("a single coincidental bigram must not count as a match, got %d hits", len(hits))
	}
}

// Coverage is computed against the query's distinct tokens, so a query that is
// mostly noise still needs its real terms to be present.
func TestCoverageUsesDistinctQueryTokens(t *testing.T) {
	ix := New()
	ix.Add("p@b/doc1", "端口 策略 端口 策略 端口")

	// Duplicates in the query must not inflate or dilute the ratio.
	if hits := ix.SearchCovered("端口 端口 端口", 3, 0, 0.5, nil, nil); len(hits) == 0 {
		t.Fatal("repeated query terms should still match")
	}
}

// A threshold of 0 disables the filter, preserving the old permissive behavior.
func TestCoverageZeroDisablesFilter(t *testing.T) {
	ix := New()
	ix.Add("p@b/doc1", "腾讯云 Windows Server 的计算机算机资源")

	permissive := ix.SearchCovered("量子计算机退相干", 3, 0, 0, nil, nil)
	strict := ix.SearchCovered("量子计算机退相干", 3, 0, 0.5, nil, nil)
	if len(permissive) == 0 {
		t.Fatal("threshold 0 should keep the old permissive behavior")
	}
	if len(strict) != 0 {
		t.Fatalf("threshold 0.5 should reject the coincidence, got %d hits", len(strict))
	}
}
