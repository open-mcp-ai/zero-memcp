package server

import (
	"fmt"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/vecindex"
)

// 语义追平的 delta 必须靠 blob hash 短路，不再全量读盘。
//
// 早先它对每一条可索引的块都 readBlock + semanticText + sha256，
// 只为回答「变没变」。24,000 块 × 每 30 秒一次 = 持续的全库读盘，
// 而且它和用户的读写抢同一份磁盘。
//
// 判据不看耗时，看**读了多少块**：第二轮完全没改动时，
// 读盘数必须严格下降（由 hash 短路替代）。
func TestSemanticDeltaSkipsReadsWhenHashUnchanged(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const (
		proj = "blackegg/me"
		br   = "fact"
	)
	// 必须 commit，否则块只在脏层，语义索引看不到它们。
	for i := 0; i < 6; i++ {
		if _, err := callMCP(t, srv, tok,
			fmt.Sprintf("put %s@%s/fact/seed%d -- zzz-semantic-hash-seed-%d body", proj, br, i, i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit "+proj+"@"+br+" -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 造一个「上一轮已经嵌入过」的状态：cached 里有向量、prints 里有指纹。
	// 短路条件是三件事同时成立（有 hash / hash 没变 / 指纹与向量都在），
	// 这里必须把后两件事造出来，否则测的就不是短路。
	ix := vecindex.New(8)
	prints := map[string]string{}
	for _, b := range srv.allIndexableBlocks() {
		if !isSemanticContent(b.Path) {
			continue
		}
		id := b.ID()
		data, err := srv.readBlock(b)
		if err != nil {
			continue
		}
		text := semanticText(string(data))
		if text == "" {
			continue
		}
		prints[id] = fingerprint(text)
		if err := ix.Add(id, make([]float32, 8)); err != nil {
			t.Fatalf("add vector: %v", err)
		}
	}
	if len(prints) == 0 {
		t.Fatal("没有可索引的语义块，测试前提不成立")
	}

	// 第一轮：旁路表为空 → 全部走读盘（把 blobHashes 填上）。
	d1 := srv.semanticDelta(ix, prints)
	if d1.skippedByHash != 0 {
		t.Fatalf("第一轮不可能有短路（旁路表是空的），却跳过了 %d 个", d1.skippedByHash)
	}
	if d1.reusable+d1.needsEmbed == 0 {
		t.Fatal("第一轮应当看到若干可索引的块")
	}
	if d1.needsEmbed != 0 || d1.reusable != len(prints) {
		t.Fatalf("第一轮指纹与缓存都齐全，不该有需要重算的：%+v（期望 reusable=%d）", d1, len(prints))
	}

	// 第二轮：什么都没改，应当有块靠 hash 短路。
	d2 := srv.semanticDelta(ix, prints)
	if d2.skippedByHash != len(prints) {
		t.Fatalf("第二轮完全没有改动，%d 个块应当全部走 hash 短路，实际 %d（第一轮 %+v，第二轮 %+v）",
			len(prints), d2.skippedByHash, d1, d2)
	}
	if d2.needsEmbed != 0 {
		t.Fatalf("没有改动却判定需要重算：%+v", d2)
	}

	// 第三轮：改一个文件，它必须重新读盘（不能被 hash 短路骗过去）。
	// 覆盖**已存在的那个块**。注意 put 的路径是「树路径前缀」，
	// 它会在 fact/seed0/ 下追加日期切片，所以不能拿它来测「同一 path 变了」。
	var victim string
	for _, b := range srv.allIndexableBlocks() {
		if isSemanticContent(b.Path) && b.Branch == br && b.Project == proj {
			victim = b.Path
			break
		}
	}
	if victim == "" {
		t.Fatal("找不到一个已存在的语义块来改")
	}
	if _, err := callMCPVars(t, srv, tok, proj+"@"+br+":"+victim,
		"zzz-semantic-hash-seed CHANGED body 这是一次真实的覆盖"); err != nil {
		t.Fatalf("modify %s: %v", victim, err)
	}
	if _, err := callMCP(t, srv, tok, "commit "+proj+"@"+br+" -m change"); err != nil {
		t.Fatalf("commit change: %v", err)
	}
	d3 := srv.semanticDelta(ix, prints)
	if d3.skippedByHash >= d2.skippedByHash {
		t.Fatalf("改了文件之后短路数没有下降，说明 hash 没跟着更新（d2=%+v d3=%+v）", d2, d3)
	}
	if d3.needsEmbed == 0 {
		t.Fatalf("改了内容却没有判定需要重算（d3=%+v）——这正是「改了但索引不更新」", d3)
	}
}

// 旁路表必须只加速、不改判据：hash 相同时结果与「老老实实读盘」一致。
func TestSemanticDeltaHashBypassDoesNotChangeVerdict(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const (
		proj = "blackegg/me"
		br   = "fact"
	)
	for i := 0; i < 4; i++ {
		if _, err := callMCP(t, srv, tok,
			fmt.Sprintf("put %s@%s/fact/v%d -- zzz-verdict-%d", proj, br, i, i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := callMCP(t, srv, tok, "commit "+proj+"@"+br+" -m seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 第一次（无旁路表）
	noBypass := srv.semanticDelta(nil, nil)

	// 清空旁路表，再跑一次「第一轮」形态
	srv.semantic.mu.Lock()
	srv.semantic.blobHashes = nil
	srv.semantic.mu.Unlock()
	again := srv.semanticDelta(nil, nil)

	if noBypass.needsEmbed != again.needsEmbed || noBypass.reusable != again.reusable {
		t.Fatalf("两次「无旁路」的结果应当一致：%+v vs %+v", noBypass, again)
	}

	// 有了旁路表之后，判定结果（needsEmbed + reusable）也必须一致，
	// 只是多了一条 skippedByHash 的加速路径。
	srv.semantic.mu.Lock()
	srv.semantic.blobHashes = nil
	srv.semantic.mu.Unlock()
	first := srv.semanticDelta(nil, nil)
	second := srv.semanticDelta(nil, nil)
	if first.needsEmbed+first.reusable != second.needsEmbed+second.reusable {
		t.Fatalf("旁路让判定结果变了：%+v vs %+v", first, second)
	}
}