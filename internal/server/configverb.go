package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// execConfig 实现 config 动词：通过 MCP 读写配置文件。
//
//	config show              打印当前配置文件全文
//	config path              打印配置文件路径
//	config put <TOML>        整体替换配置（先校验，通过才写盘并生效）
//
// 为什么要有这个动词：改配置原本必须登录机器、编辑文件、重启服务。
// 那三步里每一步都可能出错，而其中最糟的是「改了一半就重启」——
// 服务带着半份配置起来，然后行为变得没人说得清。
//
// 这个动词的顺序是刻意的：先在内存里解析校验，只有解析通过才写盘，
// 写盘成功才替换运行期索引。任何一步失败，磁盘和内存都还是旧的。
// 所以「写坏配置」这件事不会发生——坏的那份根本进不了磁盘。
func (s *Server) execConfig(rest string, c Caller) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", fmt.Errorf("用法：config show | config path | config put <TOML 全文>")
	}

	sub, body, _ := strings.Cut(rest, " ")
	sub = strings.TrimSpace(sub)
	body = strings.TrimSpace(body)

	switch sub {
	case "path":
		if s.cfg.ConfigFile == "" {
			return "（没有配置文件：本实例用命令行参数启动）", nil
		}
		abs, _ := filepath.Abs(s.cfg.ConfigFile)
		return abs, nil

	case "show":
		if s.cfg.ConfigFile == "" {
			return "（没有配置文件：本实例用命令行参数启动）", nil
		}
		data, err := os.ReadFile(s.cfg.ConfigFile)
		if err != nil {
			return "", err
		}
		return string(data), nil

	case "put":
		return s.putConfig(body)

	default:
		return "", fmt.Errorf("未知子命令 %q（可用：show / path / put）", sub)
	}
}

// putConfig 校验、写盘、生效。三步全成功才返回成功。
func (s *Server) putConfig(body string) (string, error) {
	if s.cfg.ConfigFile == "" {
		return "", errors.New("本实例没有配置文件路径，无法写入（请用 -config 启动）")
	}
	if strings.TrimSpace(body) == "" {
		return "", errors.New("配置内容为空；要清空请显式写一份最小配置")
	}

	// ① 先解析。这一步只碰内存，坏配置在这里被挡住，磁盘还是旧的。
	if _, err := config.ParseFile([]byte(body)); err != nil {
		return "", fmt.Errorf("配置有误，未写入（原配置保持不变）：%s: %w", s.cfg.ConfigFile, err)
	}

	// ② 写临时文件 + fsync + rename。
	//
	// 不能直接覆盖：写到一半断电会留下一份半截配置，
	// 而下次启动会按那份半截配置跑起来。
	dir := filepath.Dir(s.cfg.ConfigFile)
	tmp, err := os.CreateTemp(dir, ".zero-memcp-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, s.cfg.ConfigFile); err != nil {
		return "", err
	}

	// ③ 重新加载。到这里磁盘已经是新配置了；
	// 重载失败说明新配置在当前环境下不适用（比如端口被占），
	// 这时 J 报告错误，但文件已经落盘——调用方可以用 config put 写回去。
	if err := s.Reload(); err != nil {
		return "", fmt.Errorf("配置已写入，但重载失败：%w", err)
	}
	return fmt.Sprintf("配置已写入 %s 并生效", s.cfg.ConfigFile), nil
}
