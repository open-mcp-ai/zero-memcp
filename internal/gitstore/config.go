package gitstore

import (
	"bufio"
	"bytes"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MemcpConfig 是存储在 <repo>.git/config 里 [memcp] 分节的仓库专属策略。
type MemcpConfig struct {
	// CommitInterval 自动提交时间间隔，例如 "5s", "1m"。
	CommitInterval time.Duration
	// CommitCount 攒够多少条就立刻触发提交。
	CommitCount int
	// GCInterval 垃圾回收周期，例如 "24h"。
	GCInterval time.Duration
	// RuleLock 是否锁定 SKILL.md，锁定后只有 root 能够修改。
	RuleLock bool
}

func (r *Repo) configPath() string {
	return filepath.Join(r.Path, "config")
}

// Config 读取当前裸仓库的一项 git 配置，格式 "section.key"。
//
// 纯标准库文本解析，不调 git 命令。
func (r *Repo) Config(key string) string {
	sec, prop, ok := strings.Cut(key, ".")
	if !ok {
		return ""
	}
	f, err := r.FS().Open(r.configPath())
	if err != nil {
		return ""
	}
	defer f.Close()

	curSec := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			curSec = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if strings.EqualFold(curSec, sec) {
			k, v, found := strings.Cut(line, "=")
			if found && strings.EqualFold(strings.TrimSpace(k), prop) {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// SetConfig 设置一项配置。不存在时追加分节与属性。
func (r *Repo) SetConfig(key, value string) error {
	sec, prop, ok := strings.Cut(key, ".")
	if !ok {
		return fmt.Errorf("键应为 section.key，得到 %q", key)
	}
	p := r.configPath()
	raw, _ := r.FS().ReadFile(p)

	lines := strings.Split(string(raw), "\n")
	curSec := ""
	secFound := false
	propFound := false
	secInsertIdx := -1

	var out []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if strings.EqualFold(curSec, sec) && !propFound {
				// 刚离开目标分节但没找到 prop：在这里插入
				out = append(out, fmt.Sprintf("\t%s = %s", prop, value))
				propFound = true
			}
			curSec = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if strings.EqualFold(curSec, sec) {
				secFound = true
				secInsertIdx = i
			}
		} else if strings.EqualFold(curSec, sec) {
			k, _, found := strings.Cut(trimmed, "=")
			if found && strings.EqualFold(strings.TrimSpace(k), prop) {
				out = append(out, fmt.Sprintf("\t%s = %s", prop, value))
				propFound = true
				continue
			}
		}
		out = append(out, line)
	}

	if !secFound {
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out, fmt.Sprintf("[%s]", sec))
		out = append(out, fmt.Sprintf("\t%s = %s", prop, value))
	} else if !propFound {
		_ = secInsertIdx
		out = append(out, fmt.Sprintf("\t%s = %s", prop, value))
	}

	var buf bytes.Buffer
	for i, l := range out {
		buf.WriteString(l)
		if i < len(out)-1 || strings.HasSuffix(string(raw), "\n") {
			buf.WriteByte('\n')
		}
	}
	return r.FS().WriteAtomic(p, buf.Bytes(), 0o644)
}

// MemcpConfig 读出当前裸仓库的 [memcp] 自定义配置。
func (r *Repo) MemcpConfig() MemcpConfig {
	cfg := MemcpConfig{}

	if s := r.Config("memcp.commitInterval"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= 0 {
			cfg.CommitInterval = d
		}
	}
	if s := r.Config("memcp.commitCount"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			cfg.CommitCount = n
		}
	}
	if s := r.Config("memcp.gcInterval"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= 0 {
			cfg.GCInterval = d
		}
	}
	if s := r.Config("memcp.ruleLock"); s != "" {
		cfg.RuleLock = strings.EqualFold(s, "true") || s == "1"
	}

	return cfg
}
