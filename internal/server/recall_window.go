package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// timeWindow 是 recall 的时间范围：只在这个区间里找。
//
// 为什么需要它（而不是靠"降权"）：本会话刚聊过的内容**已经在上下文里**
// 了，召回它等于把手里已有的东西再读一遍——纯噪声。降权是"查出来再压
// 低分"，正确方向是**根本不进入候选集**。
//
// 之前只有 --scope（按树收窄），所以「别查刚才那段」只能靠人肉读结果
// 再跳过。时间维度是另一根轴的收窄，两者可叠加。
//
// 零值表示该侧无界。时间单位是 Unix 秒，与索引里 docTime 同构。
type timeWindow struct {
	// before 是上界（不含）：只保留严格早于它的块。
	before int64
	// after 是下界（含）：只保留不早于它的块。
	after int64
}

func (w timeWindow) set() bool { return w.before != 0 || w.after != 0 }

// keep 判断一个块的写入时间是否落在窗口内。
//
// 时间未知（t == 0）时**保留**。理由：拿不到时间就只能承认"不知道"，
// 而把它当成"太新"删掉，等于让缺元数据的旧数据从检索里消失——
// 那是比"多查一条近期内容"严重得多的错误。新写入的块一定有
// 时间（put 时 AddWithTime 带 now），所以这条只会影响来历不明的块。
func (w timeWindow) keep(t int64) bool {
	if !w.set() {
		return true
	}
	if t == 0 {
		return true
	}
	if w.before != 0 && t >= w.before {
		return false
	}
	if w.after != 0 && t < w.after {
		return false
	}
	return true
}

// parseTimeWindow 从 recall 的参数串里摘出 --before / --older-than。
//
// 与 --scope 一样就地解析而不是走 term 的 flag 机制：这个动词的
// Flags 声明里只有 boolean flag 是"不带值"的，带值 flag 的解析
// 散在动词内部，跟着既有写法走比另起一套更不容易漂。
//
// 支持的写法：
//
//	--before 2026-11-27            （日期，当天 00:00 为界，不含）
//	--before 2026-11-27T12:00:00Z  （RFC3339）
//	--older-than 2h                （比"现在"早至少 2 小时）
//	--older-than 3d / 90m / 45s
//
// 解析失败必须报错而不是忽略：静默当成"没给窗"会让调用方以为
// 自己限定过范围，实际查了全库——错的是结论，不是输出。
func parseTimeWindow(rest string, now time.Time) (string, timeWindow, error) {
	var w timeWindow
	fields := strings.Fields(rest)
	kept := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		isFlag := func(name string) bool { return f == name }
		if !isFlag("--before") && !isFlag("--older-than") && !isFlag("--after") {
			kept = append(kept, f)
			continue
		}
		if i+1 >= len(fields) {
			return "", timeWindow{}, fmt.Errorf("%s needs a value (e.g. %s %s)",
				f, f, exampleFor(f))
		}
		val := fields[i+1]
		i++
		switch f {
		case "--before":
			t, err := parseInstant(val, now)
			if err != nil {
				return "", timeWindow{}, fmt.Errorf("--before %q: %w", val, err)
			}
			w.before = t
		case "--after":
			t, err := parseInstant(val, now)
			if err != nil {
				return "", timeWindow{}, fmt.Errorf("--after %q: %w", val, err)
			}
			w.after = t
		case "--older-than":
			d, err := parseAge(val)
			if err != nil {
				return "", timeWindow{}, fmt.Errorf("--older-than %q: %w", val, err)
			}
			// 「比现在早至少 d」= 写入时间 <= now-d。用 <= 而不是 <，
			// 因为恰好 d 之前的那条应当算"够旧"。
			w.before = now.Add(-d).Unix() + 1
		}
	}
	return strings.Join(kept, " "), w, nil
}

func exampleFor(flag string) string {
	if flag == "--older-than" {
		return "2h"
	}
	return "2026-11-27"
}

// parseInstant 接受日期或 RFC3339，都按本地时区解释（人类说的"昨天"
// 是自己时钟上的昨天，不是 UTC 的）。
func parseInstant(v string, now time.Time) (int64, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, now.Location()); err == nil {
			return t.Unix(), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", v, now.Location()); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("empty value")
}

// parseAge 解析 2h / 3d / 90m / 45s 这类时长。
//
// 不直接用 time.ParseDuration：它不支持 d（天），而"三天前的"是
// 这个功能最常用的说法之一。支持 d 比要求用户写 72h 更不容易错。
func parseAge(v string) (time.Duration, error) {
	s := strings.TrimSpace(strings.ToLower(v))
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	unit := s[len(s)-1]
	numPart := s[:len(s)-1]
	mult := time.Duration(0)
	switch unit {
	case 's':
		mult = time.Second
	case 'm':
		mult = time.Minute
	case 'h':
		mult = time.Hour
	case 'd':
		mult = 24 * time.Hour
	case 'w':
		mult = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("unknown unit %q (use s/m/h/d/w)", string(unit))
	}
	n, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", numPart)
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return time.Duration(n * float64(mult)), nil
}
