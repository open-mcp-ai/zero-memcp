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
		got, gerr := srv.currentContentTree(project, branch)
		if gerr != nil {
			t.Fatalf("算内容树失败: %v", gerr)
		}
		want, werr := flattenBranchTree(repo, branch) // 绕开缓存
		if werr != nil {
			t.Fatalf("直接算内容树失败: %v", werr)
		}
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
	before, _ := srv.currentContentTree(project, branch)
	if err := repo.Write(branch, "a.md", []byte("一改"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	check("改内容（条数不变）")
	if after, _ := srv.currentContentTree(project, branch); after == before {
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
	first, _ := srv.currentContentTree(project, branch)
	if first == "" {
		t.Fatal("内容树不该为空")
	}
	second, _ := srv.currentContentTree(project, branch)
	if first != second {
		t.Fatalf("同一状态两次结果不同：%q vs %q", first, second)
	}
	if got := srv.contentTrees.hits.Load() - hits; got < 1 {
		t.Fatalf("第二次调用必须命中缓存，实际命中 %d 次（缓存没生效）", got)
	}
}

// 空分支必须返回「标准空树 + 没有错误」，不能返回 ""。
//
// "" 是「算不出来」，空树是「一条内容都没有」——两者混在一起会出人命：
// 下游的空树安全阀把「算不出来」当成「本端空了」，于是要么发空快照抹掉
// 对端，要么被挡成永久 500（实测：删掉分支上唯一一条内容后，对端每 3 秒
// 重试一次、每次都失败，此后新写的内容也一起堵死）。
//
// 这条测试钉的是「空是一个事实，不是一次缺席」。
func TestEmptyBranchIsAFactNotAnAbsence(t *testing.T) {
	srv, _ := setupMountServer(t)
	const project, branch = "blackegg/me", "main"
	repo, err := srv.projects.Ensure(project)
	if err != nil {
		t.Fatal(err)
	}
	flat, ferr := repo.FlattenBranch(branch)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(flat) != 0 {
		t.Fatalf("前置条件：这条分支应该是空的，实际 %d 条", len(flat))
	}

	tree, gerr := srv.currentContentTree(project, branch)
	if gerr != nil {
		t.Fatalf("空分支不是错误：%v", gerr)
	}
	if tree == "" {
		t.Fatal("空分支必须返回标准空树 SHA，不能返回空串——空串是「算不出来」")
	}
	if tree != gitstore.EmptyTreeSHA {
		t.Fatalf("空分支应返回标准空树 %s，得到 %s", gitstore.EmptyTreeSHA, tree)
	}

	direct, derr := flattenBranchTree(repo, branch)
	if derr != nil {
		t.Fatalf("直接算空分支失败：%v", derr)
	}
	if direct != gitstore.EmptyTreeSHA {
		t.Fatalf("直接算空分支应得标准空树，得到 %s", direct)
	}
}
