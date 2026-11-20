package gitstore

import (
	"strings"
	"testing"
)

// 世界树改写必须做到：目标内容从**整条历史**里消失，其它内容一字不动。
//
// 这是「数据的销毁权」那个契约的可执行版本。判据不能是「当前 HEAD 里
// 看不到」——那只要删一个路径就够，属于遮眼法。判据是：把每一个
// commit 的树都展开，逐条比对 blob 哈希，目标必须一个都不剩。
func TestEraseBlobRemovesFromAllHistory(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}

	const secret = "手机号 13800138000 别人的隐私"
	for i := 0; i < 3; i++ {
		if err := r.Write("zero", "2026-11-19/keep"+string(rune('a'+i))+".md", []byte("正常内容 "+string(rune('a'+i))), ModeContent); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit("zero", "add "+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Write("zero", "2026-11-19/secret.md", []byte(secret), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "oops: leaked secret"); err != nil {
		t.Fatal(err)
	}

	// 找到那个 blob
	entry, found, err := r.Stat("zero", "2026-11-19/secret.md")
	if err != nil || !found {
		t.Fatalf("secret 未写入: found=%v err=%v", found, err)
	}
	target := entry.Hash

	rep, err := r.EraseBlob("zero", target, nil)
	if err != nil {
		t.Fatalf("EraseBlob: %v", err)
	}
	if rep.RewrittenCommits == 0 {
		t.Fatal("应当重写了至少一个 commit")
	}
	t.Logf("改写结果: commits=%d paths=%v looseRemoved=%v inPack=%v",
		rep.RewrittenCommits, rep.TouchedPaths, rep.LooseRemoved, rep.InPack)

	// 判据一：当前树里没有了
	if _, found, _ := r.Stat("zero", "2026-11-19/secret.md"); found {
		t.Fatal("抹除后当前树里不应再有该路径")
	}
	// 判据二：整条历史里没有任何 commit 的树还引用这个 blob
	head, err := r.Resolve("zero")
	if err != nil || head == "" {
		t.Fatalf("resolve head: %v", err)
	}
	order, commits, err := r.collectCommitChain(head)
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range order {
		paths := map[string]Entry{}
		if err := r.loadTree(commits[sha].Tree, "", paths); err != nil {
			t.Fatalf("load tree of %s: %v", sha, err)
		}
		for p, e := range paths {
			if e.Hash == target {
				t.Fatalf("历史 commit %s 仍引用被抹除的 blob（路径 %s）——抹除失败", sha[:8], p)
			}
		}
	}
	// 判据三：其它内容完好
	for i := 0; i < 3; i++ {
		path := "2026-11-19/keep" + string(rune('a'+i)) + ".md"
		e, found, err := r.Stat("zero", path)
		if err != nil || !found {
			t.Fatalf("无辜内容 %s 被误删: found=%v err=%v", path, found, err)
		}
		body, err := r.BlobBody(e.Hash)
		if err != nil {
			t.Fatalf("读回 %s: %v", path, err)
		}
		if !strings.Contains(string(body), "正常内容") {
			t.Fatalf("%s 内容被改动: %q", path, string(body))
		}
	}
}

// 替换（R）语义：内容换成新值，路径与历史位置保留。
func TestEraseBlobReplaceKeepsPath(t *testing.T) {
	dir := t.TempDir()
	r, err := Init(dir + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write("zero", "2026-11-19/x.md", []byte("旧的不该留"), ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("zero", "old"); err != nil {
		t.Fatal(err)
	}
	e, _, _ := r.Stat("zero", "2026-11-19/x.md")

	rep, err := r.EraseBlob("zero", e.Hash, []byte("[已抹除]"))
	if err != nil {
		t.Fatalf("EraseBlob replace: %v", err)
	}
	if rep.ReplacementBlob == "" {
		t.Fatal("替换模式应记录替换后的 blob")
	}
	got, found, err := r.Stat("zero", "2026-11-19/x.md")
	if err != nil || !found {
		t.Fatalf("替换后路径应保留: found=%v err=%v", found, err)
	}
	body, _ := r.BlobBody(got.Hash)
	if string(body) != "[已抹除]" {
		t.Fatalf("内容应为替换值，实际 %q", string(body))
	}
	if got.Hash == e.Hash {
		t.Fatal("替换后不应还是原来的 blob")
	}
}
