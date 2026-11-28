package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 内容树缓存不许改变答案。
//
// 缓存键取「已提交树 + 脏层指纹」，所以任何一次改动都必须换键。
// 这里把四类改动逐条走一遍，每次都与「绕开缓存直接算」的结果比对。
//
// 第 ② 条是重点：把一个已有路径改成新内容，脏层**条数不变**——
// 只记「脏层有几条」的实现会在这里静默给出过期答案，而这正是
// 「靠失效逻辑」那条路会踩的坑，所以键取的是内容指纹。
func TestContentTreeCacheNeverChangesTheAnswer(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "main"
	repo, err := srv.projects.Ensure(project)
	if err != nil {
		t.Fatal(err)
	}

	check := func(what string) {
		t.Helper()
		got := srv.currentContentTree(project, branch)
		want := flattenBranchTree(repo, branch) // 绕开缓存
		if got != want {
			t.Fatalf("%s：缓存给出 %q，直接算得到 %q", what, got, want)
		}
	}

	check("空分支")

	// ① 新写（脏层多一条）
	if err := repo.Write(branch, "a.md", []byte("一"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	check("新写一条")
	if err := repo.Write(branch, "b.md", []byte("二"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	check("再写一条")

	// ② 改已有路径的内容：脏层条数不变，树必须变
	before := srv.currentContentTree(project, branch)
	if err := repo.Write(branch, "a.md", []byte("一改"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	check("改内容（条数不变）")
	if after := srv.currentContentTree(project, branch); after == before {
		t.Fatal("改了内容但内容树没变——缓存给了过期答案")
	}

	// ③ 提交（已提交树变，脏层清空）
	if _, err := repo.Commit(branch, "seed"); err != nil {
		t.Fatal(err)
	}
	check("提交")

	// ④ 删除（墓碑）
	if err := repo.Delete(branch, "b.md"); err != nil {
		t.Fatal(err)
	}
	check("删除")
}

// 同一个状态问第二遍必须走缓存，而不是从头再算一遍。
//
// 为什么这条要断言命中计数而不是耗时：耗时断言在慢机器上会假红，
// 而「有没有命中」是事实，不是测量。
func TestContentTreeCacheHitsOnRepeat(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "main"
	repo, err := srv.projects.Ensure(project)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"a.md", "b.md", "c.md"} {
		if err := repo.Write(branch, p, []byte{byte('0' + i)}, gitstore.ModeContent); err != nil {
			t.Fatal(err)
		}
	}

	hits := srv.contentTrees.hits.Load()
	first := srv.currentContentTree(project, branch)
	if first == "" {
		t.Fatal("内容树不该为空")
	}
	second := srv.currentContentTree(project, branch)
	if first != second {
		t.Fatalf("同一状态两次结果不同：%q vs %q", first, second)
	}
	if got := srv.contentTrees.hits.Load() - hits; got < 1 {
		t.Fatalf("第二次调用必须命中缓存，实际命中 %d 次（缓存没生效）", got)
	}
}
