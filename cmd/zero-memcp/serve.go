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
	"github.com/open-mcp-ai/zero-memcp/internal/weights"
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
		weightsDir  = fs.String("weights", "", "Read-frequency state directory (default: $HOME/.zero-memcp/weights); a legacy weights.json path is migrated automatically")
		embedLib    = fs.String("embed-lib", "", "Path to libonnxruntime (enables semantic search when model+vocab also given)")
		embedModel  = fs.String("embed-model", "", "Path to ONNX embedding model (e.g. bge-small-zh-int8.onnx)")
		embedVocab  = fs.String("embed-vocab", "", "Path to BERT vocab.txt for the embedding model")
		vecDir      = fs.String("vector-dir", "", "Vector cache directory (default: <data>/vectors)")
	)
	_ = fs.Parse(args)

	data := expandHome(*dataDir)
	cfgPth := expandHome(*configFile)
	if data == "" && cfgPth == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: specify at least one of -data or -config")
		return 2
	}

	// 权重状态：从「单文件」改成「按项目分片的目录」。
	//
	// 旧配置可能还写着 weights.json（文件路径）。遇到这种情况：
	// 把它的同目录 weights/ 作为分片目录，并在启动前把旧文件迁进去。
	// 迁移完成后旧文件改名 .migrated，保证只跑一次（迁移是 sum 语义，
	// 幂等性靠退役机制保证而不是靠合并语义）。
	wDir := expandHome(*weightsDir)
	if wDir == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			wDir = filepath.Join(home, ".zero-memcp", "weights")
		}
	}
	var legacyWeights string
	if strings.HasSuffix(wDir, ".json") {
		legacyWeights = wDir
		wDir = filepath.Join(filepath.Dir(wDir), "weights")
	} else if home, _ := os.UserHomeDir(); home != "" {
		// 默认位置上的旧文件也要迁
		cand := filepath.Join(home, ".zero-memcp", "weights.json")
		if _, err := os.Stat(cand); err == nil {
			legacyWeights = cand
		}
	}
	if legacyWeights != "" {
		if n, dropped, err := weights.MigrateLegacy(legacyWeights, wDir); err != nil {
			fmt.Fprintf(os.Stderr, "zero-memcp: migrate legacy weights %s: %v\n", legacyWeights, err)
		} else {
			fmt.Fprintf(os.Stderr, "zero-memcp: migrated %d weight entries (%d bare duplicates dropped) from %s into %s\n",
				n, dropped, legacyWeights, wDir)
		}
	}

	branch := *defBranch

	cfg := server.Config{
		DataDir:       data,
		ConfigFile:    cfgPth,
		DefaultBranch: branch,
		Endpoint:      *endpoint,
		WeightsDir:    wDir,
	}
	cfg.Semantic.LibPath = expandHome(*embedLib)
	cfg.Semantic.ModelPath = expandHome(*embedModel)
	cfg.Semantic.VocabPath = expandHome(*embedVocab)
	cfg.Semantic.VectorDir = expandHome(*vecDir)
	// 语义检索资产：命令行优先，配置文件其次。
	//
	// 为什么配置优先于命令行缺省：模型路径是「装好就不动」的本机资产，
	// 每次启动都背四个长参数不现实。但命令行必须能覆盖它——
	// 换模型做对比实验时不该去改全局配置。
	if cfgPth != "" {
		if f, err := config.Load(cfgPth); err == nil && f != nil {
			if cfg.Semantic.LibPath == "" {
				cfg.Semantic.LibPath = expandHome(f.Semantic.Lib)
			}
			if cfg.Semantic.ModelPath == "" {
				cfg.Semantic.ModelPath = expandHome(f.Semantic.Model)
			}
			if cfg.Semantic.VocabPath == "" {
				cfg.Semantic.VocabPath = expandHome(f.Semantic.Vocab)
			}
			if cfg.Semantic.VectorDir == "" {
				cfg.Semantic.VectorDir = expandHome(f.Semantic.Vectors)
			}
		}
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
	for _, u := range srv.AdminURLs() {
		fmt.Fprintf(os.Stderr, "  WebUI: %s\n", u)
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
