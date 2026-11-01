package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/open-mcp-ai/zero-memcp/internal/server"
)

// runServe 是默认行为：起服务。
func runServe(args []string) int {
	fs := flag.NewFlagSet("zero-memcp", flag.ExitOnError)
	var (
		dataDir     = fs.String("data", "", "数据根目录（system/ 与 repos/ 都在它下面）")
		configFile  = fs.String("config", "", "配置文件（用户/组/项目/权限与自动执行间隔）")
		defBranch   = fs.String("branch", "", "默认分支（缺省取配置或内置默认）")
		listen      = fs.String("listen", "", "监听地址，缺省 127.0.0.1:1729")
		endpoint    = fs.String("endpoint", "", "本端点标识，例如 blackbox / laptop")
		dump        = fs.Bool("dump", false, "只打印扫描结果后退出，不启动服务")
		weightsPath = fs.String("weights", "", "阅读频次状态文件，缺省 $HOME/.zero-memcp/weights.json")
	)
	_ = fs.Parse(args)

	data := expandHome(*dataDir)
	cfgPth := expandHome(*configFile)
	if data == "" && cfgPth == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 至少需要一个 -data 或 -config")
		return 2
	}

	wPath := *weightsPath
	if wPath == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			wPath = filepath.Join(home, ".zero-memcp", "weights.json")
		}
	} else {
		wPath = expandHome(wPath)
	}

	branch := *defBranch

	cfg := server.Config{
		DataDir:       data,
		ConfigFile:    cfgPth,
		DefaultBranch: branch,
		Endpoint:      *endpoint,
		WeightsPath:   wPath,
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	srv, err := server.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 1
	}

	if *dump {
		srv.Report(os.Stdout, server.Caller{User: "root", Root: true, Cap: "rwx"})
		return 0
	}

	// 收到信号时关闭监听，让 Start 返回。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		_ = srv.Close()
	}()

	for _, addr := range srv.ListenAddrs() {
		fmt.Fprintf(os.Stderr, "zero-memcp: 监听 %s（%s）\n", addr, srv.DescribeProjects())
	}
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 1
	}
	return 0
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := expandHome(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
