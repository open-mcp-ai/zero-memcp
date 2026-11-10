package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/daemon"
)

func runDaemon(args []string) {
	if len(args) == 0 {
		daemonHelp()
		os.Exit(1)
	}

	verb := args[0]
	rest := args[1:]

	fs := flag.NewFlagSet("zero-memcp daemon", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to zero-memcp.toml")
	dataDir := fs.String("data", "", "Data directory (defaults to ~/.zero-memcp/data)")
	healthURL := fs.String("health", "http://127.0.0.1:1729/health", "Health endpoint URL")
	_ = fs.Parse(rest)

	dir := resolveDataDir(*dataDir, *configPath)

	switch verb {
	case "status":
		st := daemon.Inspect(dir, *healthURL)
		fmt.Println(st.String())
		if !st.Alive {
			os.Exit(1)
		}

	case "stop":
		if err := daemon.Stop(dir); err != nil {
			fmt.Fprintf(os.Stderr, "zero-memcp daemon: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("实例已停止")

	case "start":
		st := daemon.Inspect(dir, *healthURL)
		if st.Alive {
			fmt.Printf("实例已在运行（pid=%d）\n", st.Pid)
			return
		}
		if err := spawnDaemon(*configPath, *dataDir, dir); err != nil {
			fmt.Fprintf(os.Stderr, "zero-memcp daemon start: %v\n", err)
			os.Exit(1)
		}
		// 等它就绪
		for i := 0; i < 40; i++ {
			time.Sleep(100 * time.Millisecond)
			st = daemon.Inspect(dir, *healthURL)
			if st.Alive && st.Healthy {
				fmt.Printf("实例已在后台启动（pid=%d，耗时 %dms）\n", st.Pid, st.HealthMS)
				return
			}
		}
		fmt.Fprintf(os.Stderr, "子进程已拉起但 4 秒内未能通过健康检查；查看日志排查\n")
		os.Exit(1)

	case "help", "-h", "--help":
		daemonHelp()

	default:
		fmt.Fprintf(os.Stderr, "未知 daemon 动作: %s\n", verb)
		daemonHelp()
		os.Exit(1)
	}
}

func daemonHelp() {
	fmt.Println(`zero-memcp daemon —— 管理后台守护进程

用法:
  zero-memcp daemon start  [-config <path>] [-data <path>]
  zero-memcp daemon stop   [-data <path>]
  zero-memcp daemon status [-data <path>] [-health <url>]

说明:
  start  拉起脱离终端的后台实例，将 pid 写入 <data>/daemon.pid
  stop   向后台实例发 SIGTERM 优雅退出（Windows 上强杀），清理 pidfile
  status 检查 pid 是否存活并探测 /health`)
}

func resolveDataDir(explicitData, configPath string) string {
	if explicitData != "" {
		return explicitData
	}
	// 只给了 -config 时，数据目录必须从配置文件里读。
	//
	// 早先这里直接回退到 ~/.zero-memcp/data，于是 `daemon start -config X`
	// 把 pidfile 写到了 home 目录，而 `daemon status -data Y` 去 X 指的
	// 目录找——两边各看一个地方，状态永远报「未运行」而进程其实好好地跑着。
	// 这种「两处各记一份真相」的错误，症状比崩溃难查得多。
	if configPath != "" {
		if f, err := config.Load(configPath); err == nil && f.Server.Data != "" {
			return f.Server.Data
		}
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, ".zero-memcp", "data")
	}
	return "data"
}

func spawnDaemon(configPath, explicitData, dataDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self executable: %w", err)
	}

	var childArgs []string
	childArgs = append(childArgs, "serve")
	if configPath != "" {
		childArgs = append(childArgs, "-config", configPath)
	}
	if explicitData != "" {
		childArgs = append(childArgs, "-data", explicitData)
	}

	logPath := filepath.Join(dataDir, "daemon.log")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open daemon.log: %w", err)
	}

	cmd := exec.Command(exe, childArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	daemon.Detach(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start detached child: %w", err)
	}
	logFile.Close()

	if err := daemon.WritePid(dataDir, cmd.Process.Pid); err != nil {
		// 记录失败别直接崩：进程已经起了，打个警告就行
		fmt.Fprintf(os.Stderr, "warning: cannot write pidfile: %v\n", err)
	}
	return nil
}
