package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
		return "", fmt.Errorf("usage: config show | config path | config put <full TOML>")
	}

	sub, body, _ := strings.Cut(rest, " ")
	sub = strings.TrimSpace(sub)
	body = strings.TrimSpace(body)

	// termcp 重建命令行时会把 --include / --exclude / --reset 挪到位置参数
	// 前面（`config index --exclude x` 会变成 `config --exclude x index`），
	// 所以不能只看第一个词判子命令——看到 index 的 flag 也算 index。
	{
		isIndex := sub == "index"
		if !isIndex {
			for _, f := range []string{"--include", "--exclude", "--reset"} {
				if strings.Contains(" "+rest+" ", " "+f+" ") {
					isIndex = true
					break
				}
			}
		}
		if isIndex {
			if err := s.requireRootGlobal(c, 'w'); err != nil {
				return "", err
			}
			// 去掉字面的 "index" 词（它可能出现在 flag 前面也可能在后面），
			// 剩下的全部交给 setIndexScope 解析。
			var keep []string
			for _, f := range strings.Fields(rest) {
				if f == "index" {
					continue
				}
				keep = append(keep, f)
			}
			return s.setIndexScope(strings.Join(keep, " "))
		}
	}

	switch sub {
	case "path":
		if err := s.requireRootGlobal(c, 'r'); err != nil {
			return "", err
		}
		if s.cfg.ConfigFile == "" {
			return "(no config file; this instance was started with command-line arguments)", nil
		}
		abs, _ := filepath.Abs(s.cfg.ConfigFile)
		return abs, nil

	case "show":
		if err := s.requireRootGlobal(c, 'r'); err != nil {
			return "", err
		}
		if s.cfg.ConfigFile == "" {
			return "(no config file; this instance was started with command-line arguments)", nil
		}
		data, err := os.ReadFile(s.cfg.ConfigFile)
		if err != nil {
			return "", err
		}
		return string(data), nil

	case "put":
		if err := s.requireRootGlobal(c, 'w'); err != nil {
			return "", err
		}
		return s.putConfig(body)

	default:
		return "", fmt.Errorf("unknown subcommand %q (available: show / path / put)", sub)
	}
}

// putConfig 校验、写盘、生效。三步全成功才返回成功。
func (s *Server) putConfig(body string) (string, error) {
	if s.cfg.ConfigFile == "" {
		return "", errors.New("this instance has no config file path to write to (start with -config)")
	}
	if strings.TrimSpace(body) == "" {
		return "", errors.New("config content is empty; to clear it, explicitly provide a minimal config")
	}

	// ① 先解析。这一步只碰内存，坏配置在这里被挡住，磁盘还是旧的。
	if _, err := config.ParseFile([]byte(body)); err != nil {
		return "", fmt.Errorf("invalid config; not written (existing config unchanged): %s: %w", s.cfg.ConfigFile, err)
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
		return "", fmt.Errorf("config was written, but reload failed: %w", err)
	}
	return fmt.Sprintf("config written to %s and applied", s.cfg.ConfigFile), nil
}

// setIndexScope 通过 MCP 改 [index] include/exclude，改完立即生效。
//
//	config index --exclude "BlackEggAI/zero@main.chat,BlackEggAI/zero@inbox"
//	config index --include "BlackEggAI/zero@ops"
//	config index --reset
//
// 纪律：绝不用正则改配置；按行替换、写后回读校验、失败不落盘。
func (s *Server) setIndexScope(rest string) (string, error) {
	if s.cfg.ConfigFile == "" {
		return "", errors.New("this instance has no config file path to write to (start -config)")
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		f := s.cfgFilePtr.Load()
		if f == nil {
			return "index scope: (no config file)", nil
		}
		return fmt.Sprintf("include: %v\nexclude: %v\nasync: %v",
			f.Index.Include, f.Index.Exclude, f.Index.AsyncBuild()), nil
	}

	// 解析参数：--include / --exclude 取逗号分隔列表，--reset 清空两者。
	inc, exc, reset := []string(nil), []string(nil), false
	incSet, excSet := false, false
	fields := strings.Fields(rest)
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "--reset":
			reset = true
		case "--include", "--exclude":
			key := fields[i]
			var val string
			if i+1 >= len(fields) {
				return "", fmt.Errorf("%s needs a value (comma-separated project@branch patterns)", key)
			}
			val = fields[i+1]
			i++
			var list []string
			for _, part := range strings.Split(val, ",") {
				if p := strings.TrimSpace(part); p != "" {
					list = append(list, p)
				}
			}
			if key == "--include" {
				inc, incSet = list, true
			} else {
				exc, excSet = list, true
			}
		default:
			return "", fmt.Errorf("config index: unknown argument %q (supported: --include A,B | --exclude A,B | --reset)", fields[i])
		}
	}
	if incSet && excSet {
		return "", fmt.Errorf("config index: --include and --exclude cannot both be set " +
			"(a whitelist and a blacklist at once has no single answer; pick one)")
	}

	raw, err := os.ReadFile(s.cfg.ConfigFile)
	if err != nil {
		return "", err
	}
	updated, err := rewriteIndexSection(string(raw), inc, exc, incSet, excSet, reset)
	if err != nil {
		return "", err
	}
	// 先校验：坏配置绝不落盘（与 config put 同一条纪律）。
	if _, err := config.ParseFile([]byte(updated)); err != nil {
		return "", fmt.Errorf("resulting config is invalid; not written (existing config unchanged): %w", err)
	}
	if err := writeFileAtomic(s.cfg.ConfigFile, updated); err != nil {
		return "", err
	}
	if err := s.Reload(); err != nil {
		return "", fmt.Errorf("index scope written, but reload failed: %w", err)
	}
	f := s.cfgFilePtr.Load()
	return fmt.Sprintf("index scope applied (no restart): include=%v exclude=%v\n%s",
		f.Index.Include, f.Index.Exclude, s.indexProgressNow().Line()), nil
}

// rewriteIndexSection 按行重写 [index] 里的 include / exclude。
//
// 按行而不是正则：这份文件里还有权限、端口、密钥路径，一个贪婪匹配
// 改错一行的代价是服务起不来或权限放开。规矩与手工编辑一致：
// 只碰目标键所在的那一行，其余逐字保留。
func rewriteIndexSection(src string, inc, exc []string, incSet, excSet, reset bool) (string, error) {
	lines := strings.Split(src, "\n")
	section := ""
	seenIndex := false
	// indexEnd 是「[index] 节的末尾」在 out 里的下标（插入新键的位置）。
	// -1 表示还没确定（还在这一节里）。
	indexEnd := -1
	var out []string

	formatList := func(name string, list []string) string {
		quoted := make([]string, len(list))
		for i, v := range list {
			quoted[i] = strconv.Quote(v)
		}
		return name + " = [" + strings.Join(quoted, ", ") + "]"
	}
	dropOld := reset || incSet || excSet

	for _, line := range lines {
		trimmed := strings.TrimSpace(stripInlineComment(line))
		// 分节头（含 [[数组表]]）：它标志上一节到此结束。
		//
		// `[[user]]` 这种双括号头同样是边界——漏判的后果是把新键插到
		// 文件末尾、落在别的节里，那份配置直接失效（写之前会校验，
		// 所以不会落盘，但功能也就没生效）。
		if strings.HasPrefix(trimmed, "[") {
			if section == "index" && indexEnd < 0 {
				indexEnd = len(out)
			}
			section = ""
			if !strings.HasPrefix(trimmed, "[[") {
				section = strings.Trim(trimmed, "[] ")
			}
			if section == "index" {
				seenIndex = true
				indexEnd = -1
			}
			out = append(out, line)
			continue
		}
		if section == "index" && dropOld {
			if k, _, ok := strings.Cut(trimmed, "="); ok {
				key := strings.TrimSpace(k)
				if key == "include" || key == "exclude" {
					// 丢掉旧行：稍后统一写入新值。
					continue
				}
			}
		}
		out = append(out, line)
	}
	if section == "index" && indexEnd < 0 {
		indexEnd = len(out)
	}
	if !seenIndex {
		// 没有 [index] 节就补一个。enabled 是它的必填语义（不写等于关闭），
		// 所以必须显式打开——否则写了 exclude 却整体没启用。
		out = append(out, "", "[index]", "enabled = true")
		indexEnd = len(out)
	}

	var add []string
	if !reset {
		if incSet && len(inc) > 0 {
			add = append(add, formatList("include", inc))
		}
		if excSet && len(exc) > 0 {
			add = append(add, formatList("exclude", exc))
		}
	}
	if len(add) > 0 {
		if indexEnd < 0 || indexEnd > len(out) {
			indexEnd = len(out)
		}
		merged := make([]string, 0, len(out)+len(add))
		merged = append(merged, out[:indexEnd]...)
		merged = append(merged, add...)
		merged = append(merged, out[indexEnd:]...)
		out = merged
	}
	return strings.Join(out, "\n"), nil
}

// stripInlineComment 去掉行尾注释，但不碰引号里的 #。
func stripInlineComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			if inStr && i+1 < len(line) {
				i++
			}
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return line[:i]
			}
		}
	}
	return line
}

// writeFileAtomic 临时文件 + fsync + rename，与 config put 同一套写法。
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".zero-memcp-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}
