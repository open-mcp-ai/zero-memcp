package gittransport

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 浅拷贝导出：推一个没有父提交的孤儿提交，历史全部丢掉。
//
// 钉住三件事：
//   - 孤儿提交确实没有父提交（历史真的断了，不是「看起来像」）
//   - 树内容与源分支头部完全一致（断的只是历史，内容一个不少）
//   - 远端拿到的是一个单提交仓库，本地历史不受影响

func newRepoWithHistory(t *testing.T, dir string) (*gitstore.Repo, string) {
	t.Helper()
	repo, err := gitstore.Init(dir)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	// 三个提交，模拟有历史的仓库
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := repo.Write("zero", name, []byte("content of "+name), gitstore.Mode("100644")); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := repo.Commit("zero", "add "+name); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
	}
	head, err := repo.Resolve("zero")
	if err != nil || head == "" {
		t.Fatalf("resolve head: %v", err)
	}
	return repo, head
}

func TestOrphanCommitHasNoParents(t *testing.T) {
	repo, head := newRepoWithHistory(t, t.TempDir())

	orphan, tree, err := repo.CreateOrphanCommit("zero", "snapshot")
	if err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	if orphan == "" || tree == "" {
		t.Fatalf("孤儿提交与树都不该为空")
	}
	if orphan == head {
		t.Fatalf("孤儿提交不该等于分支头")
	}

	// 内容必须完全一致：断的只是历史，不是内容。
	headTree, err := repo.TreeOfCommit(head)
	if err != nil {
		t.Fatal(err)
	}
	if tree != headTree {
		t.Fatalf("快照的树必须等于分支头的树：%s vs %s", tree, headTree)
	}
}

func TestOrphanPushGivesRemoteSingleCommit(t *testing.T) {
	srcDir := t.TempDir()
	repo, head := newRepoWithHistory(t, srcDir)

	remoteDir := t.TempDir() + "/remote.git"
	if _, err := gitstore.Init(remoteDir); err != nil {
		t.Fatalf("init remote: %v", err)
	}

	// 先常规推一次：远端有了完整历史
	if _, err := PushLocal(repo, remoteDir, "zero"); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// 再推一个孤儿快照：必须允许非快进，否则会被拒
	orphan, _, err := repo.CreateOrphanCommit("zero", "snapshot export")
	if err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	res, err := PushLocal(repo, remoteDir, "zero", PushOptions{
		SourceSHA:           orphan,
		AllowNonFastForward: true,
	})
	if err != nil {
		t.Fatalf("snapshot push: %v", err)
	}
	if res.New != orphan {
		t.Fatalf("远端应指向孤儿提交 %s，实际 %s", orphan[:8], res.New[:8])
	}

	// 远端现在只有这一个提交：它的历史长度必须是 1。
	remote, err := gitstore.Open(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	remoteHead, err := remote.Resolve("zero")
	if err != nil || remoteHead == "" {
		t.Fatalf("远端分支应存在: %v", err)
	}
	if remoteHead != orphan {
		t.Fatalf("远端头应是孤儿提交")
	}
	log, err := remote.Log("zero", 0)
	if err != nil {
		t.Fatalf("读远端历史: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("浅拷贝之后远端历史必须只有 1 个提交，实际 %d 个", len(log))
	}

	// 本地历史必须原样保留——快照只影响推出去的那一份。
	localLog, err := repo.Log("zero", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(localLog) < 3 {
		t.Fatalf("本地历史不该被快照影响，应仍有 >=3 个提交，实际 %d", len(localLog))
	}
	if got, _ := repo.Resolve("zero"); got != head {
		t.Fatalf("本地分支头不该被快照改动")
	}
}

// 常规推送必须仍然拒绝非快进——快照是显式选项，不能顺手放宽所有人。
func TestNormalPushStillRejectsNonFastForward(t *testing.T) {
	srcDir := t.TempDir()
	repo, _ := newRepoWithHistory(t, srcDir)

	remoteDir := t.TempDir() + "/remote.git"
	if _, err := gitstore.Init(remoteDir); err != nil {
		t.Fatalf("init remote: %v", err)
	}
	if _, err := PushLocal(repo, remoteDir, "zero"); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// 在远端制造一条本地没有的历史：直接把远端 ref 指向别的提交。
	other, _, err := repo.CreateOrphanCommit("zero", "unrelated")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := gitstore.Open(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PushLocal(repo, remoteDir, "zero", PushOptions{SourceSHA: other, AllowNonFastForward: true}); err != nil {
		t.Fatalf("force push: %v", err)
	}
	_ = remote

	// 现在远端指向孤儿，本地分支头不是它的后代 —— 常规推送必须被拒。
	if _, err := PushLocal(repo, remoteDir, "zero"); err == nil {
		t.Fatalf("常规推送不该允许非快进改写远端历史")
	}
}