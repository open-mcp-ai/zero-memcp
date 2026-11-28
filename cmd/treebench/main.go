// treebench 量「内容树」这条路径的代价。
//
// 为什么要有这个工具（2026-11-28）：同步流水里出现「0 条变化的拉取
// 耗时 37 秒」，而 currentContentTree 每次都从零重建整棵内容树
// （weft@main.record 2.1 万条），且它挂在每一次拉取与每一次推送的路径上。
//
// 它量三件事：
//  1. 慢路径（摊平整条分支 + 重写全部 tree 对象）单次代价；
//  2. 缓存命中后的代价；
//  3. 两个结果是否一致——缓存不许改变答案。
//
// 顺带记录一个反例：曾经想「脏层为空时直接用已提交树」当快路径，
// 被这个工具否掉了——已提交树会带空目录，内容树只含文件，
// 于是 main 分支上两者内容相同而 SHA 不同。
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

func flattenTree(repo *gitstore.Repo, br string) (string, int) {
	flat, err := repo.FlattenBranch(br)
	if err != nil || len(flat) == 0 {
		return "", 0
	}
	pm := make(map[string]gitstore.Entry, len(flat))
	for _, e := range flat {
		pm[e.Path] = gitstore.Entry{Path: e.Path, Mode: e.Mode, Hash: e.SHA}
	}
	tree, _ := repo.WriteTreeFromPaths(pm)
	return tree, len(flat)
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: treebench <repo 路径> [分支...]")
		os.Exit(2)
	}
	repo, err := gitstore.Open(os.Args[1])
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	branches := os.Args[2:]
	if len(branches) == 0 {
		branches = []string{"main"}
	}
	for _, br := range branches {
		t0 := time.Now()
		tree, n := flattenTree(repo, br)
		slow := time.Since(t0)

		// 缓存命中：用同一个键再取一次，量的就是「不再摊平」的那一段。
		t1 := time.Now()
		if c, cerr := repo.Resolve(br); cerr == nil && c != "" {
			_, _ = repo.TreeOfCommit(c)
		}
		_, _ = repo.DirtyChanges(br)
		warm := time.Since(t1)

		fmt.Printf("%-14s entries=%-6d pending=%-4d 摊平重建=%9.2fms 键计算(命中时只剩这段)=%8.3fms  tree=%s\n",
			br, n, repo.Pending(br),
			float64(slow.Microseconds())/1000, float64(warm.Microseconds())/1000, short(tree))
	}
}
