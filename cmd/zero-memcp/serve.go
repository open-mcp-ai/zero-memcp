package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/server"
)

// runServe 是默认行为：起服务。
func runServe(args []string) int {
	fs := flag.NewFlagSet("zero-memcp", flag.ExitOnError)
	var (
		dataDir     = fs.String("data", "", "Data directory (contains system/ and repos/)")
		configFile  = fs.String("config", "", "Config file (users, groups, projects, permissions, maintenance)")
		defBranch   = fs.String("branch", "", "Default branch (from config or built-in default)")
		listen      = fs.String("listen", "", "Listen address (default 127.0.0.1:1729)")
		endpoint    = fs.String("endpoint", "", "Endpoint identity, e.g. blackbox or laptop")
		weightsPath = fs.String("weights", "", "Read-frequency state file (default: $HOME/.zero-memcp/weights.json)")
	)
	_ = fs.Parse(args)

	data := expandHome(*dataDir)
	cfgPth := expandHome(*configFile)
	if data == "" && cfgPth == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: specify at least one of -data or -config")
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

	// 收到信号时关闭监听，让 Start 返回。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		_ = srv.Close()
	}()

	for _, addr := range srv.ListenAddrs() {
		fmt.Fprintf(os.Stderr, "zero-memcp: listening on %s (%s)\n", addr, srv.DescribeProjects())
	}
	// 初始管理口令只打印这一次。
	//
	// 它会被写进 docker logs 之类的日志，所以它的价值不在保密，
	// 而在于一次性：第一次登录后强制改密，改完这个值当场作废。
	// 没有「强制改密」这一步，随机口令就退化成写在日志里的常驻密码。
	if pw := srv.TakeBootstrapPassword(); pw != "" {
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  Initial WebUI password: %s\n", pw)
		fmt.Fprintf(os.Stderr, "  This password is one-time and must be changed after the first login.\n")
		fmt.Fprintf(os.Stderr, "  To provide your own initial password, set ZERO_MEMCP_UI_PASS before startup.\n")
		fmt.Fprintf(os.Stderr, "\n")
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

// expandHome 保留成薄包装，只为了让命令行的调用点读起来短一点。
// 展开规则只有一份实现（config.ExpandHome）：配置文件里的 data 也走它，
// 两处规则一旦分叉，就会出现「命令行能用、配置里建出名字叫 ~ 的目录」。
func expandHome(p string) string {
	return config.ExpandHome(p)
}
