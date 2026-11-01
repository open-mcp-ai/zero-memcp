package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// runGC 打包所有裸仓库的松散对象。
func runGC(args []string) int {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	var (
		data = fs.String("data", "", "数据根目录")
		proj = fs.String("project", "", "只处理这个项目")
		dry  = fs.Bool("dry-run", false, "只列出要处理的仓库")
	)
	_ = fs.Parse(args)

	dataDir := expandHome(*data)
	if dataDir == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 必须给 -data")
		return 2
	}

	reposDir := filepath.Join(dataDir, "repos")
	entries, err := os.ReadDir(reposDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 1
	}

	rc := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ".git") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".git")
		if *proj != "" && name != *proj {
			continue
		}
		repoPath := filepath.Join(reposDir, e.Name())
		if *dry {
			fmt.Printf("会处理 %s\n", repoPath)
			continue
		}
		if err := repackRepo(repoPath); err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", repoPath, err)
			rc = 1
			continue
		}
		fmt.Printf("已打包 %s\n", repoPath)
	}
	return rc
}

// repackRepo 用内置打包器合并松散对象，不依赖系统 git。
func repackRepo(path string) error {
	repo, err := gitstore.Open(path)
	if err != nil {
		return err
	}
	_, err = repo.Repack()
	return err
}
