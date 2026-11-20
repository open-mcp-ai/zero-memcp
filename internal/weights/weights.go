// Package weights 记录「有哪些记忆真的被读过」，并把这件事变成检索上的加权。
//
// 只统计 read，不统计 recall。
//
// recall 只是列出候选，我瞄一眼没读，说明它没帮上忙。如果连"出现在候选里"都算
// 一次强化，就会出现自我强化回路——常常浮上来的越来越常浮上来，跟重不重要无关。
// read 是明确的"我用了它"，所以只有它算。
//
// # 存储结构（2026-11 重构）
//
// 旧版是单个 weights.json，两类问题：
//
//  1. 键格式三种并存。server 在 read 时同时 Bump 两个键
//     （MakeID 的新键 + proj/path 的旧键），同一份记忆的计数被劈成两半，
//     权重加成按一半算。根因是「索引键曾经过一次语法迁移」。
//  2. 无法跨机器合并。对等同步后，两台机器各自的权重无法合并——
//     黑盒 JSON 没有 merge 语义。
//
// 新结构：按项目分片 + 单一规范键。
//
//	<dir>/BlackEggAI/zero.json   ← {"version":1,"entries":{...}}
//	<dir>/BlackEggAI/weft.json
//
// 键一律是 memory.MakeID 的产物（project@branch:path），每个键只出现一次。
// 每个条目带 hits / last：hits 是累计读取数，last 是最后读取的 Unix 秒。
//
// # 合并语义（对等同步的前提）
//
// 同一个键在两台机器上分别有计数 (hitsA, lastA) 与 (hitsB, lastB)：
//
//	hits = max(hitsA, hitsB)
//	last = max(lastA, lastB)
//
// 取 max 不取 sum：计数是「这份记忆被用过的强度」，不是流水——
// 两台机器读同一篇 20 次，强度是 20 而不是 40。取 max 的另一个好处是
// 合并满足交换律与幂等性：同步多少轮、谁先谁后，结果一致，不会因为
// 反复同步而虚增。
package weights

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// formatVersion 出现在每个分片里。未来字段变更时用于识别旧格式并迁移。
const formatVersion = 1

// entry 是一条记忆的读取统计。
type entry struct {
	Hits int   `json:"hits"`
	Last int64 `json:"last"`
}

// shardFile 返回某个项目（可含斜杠，如 BlackEggAI/zero）的分片路径。
// 项目名里的 / 直接映射为目录层级，与 repos/ 目录结构同构。
func shardFile(dir, project string) string {
	return filepath.Join(dir, filepath.FromSlash(project)+".json")
}

// shard 是一个项目的权重状态。
//
// Entries 用 sync.Map：每个键（一条记忆地址）互相独立，没有跨键不变量。
// 更新一条记录是「读计数 → 加一 → 写回」，用 CompareAndSwap 重试，
// 而不是用一把全局锁把所有人的 recall 串起来——recall 是热路径。
type shard struct {
	Version int
	Entries sync.Map // addr -> entry
	dirty   atomic.Bool
}

// shardSlot 是分片的单飞占位：第一个调用者负责读盘，其余人等 done。
//
// 为什么必须单飞：loadShard 在 recall 的热路径上被调用，
// 而它要 os.ReadFile。让每个并发调用者各读一次盘是浪费，
// 把读盘塞进互斥锁又会让所有 recall 排队——单飞两者都避开。
type shardSlot struct {
	done chan struct{}
	sh   *shard
	err  error
}

// Store 是全部项目的权重集合。dir 为空表示纯内存（测试用）。
type Store struct {
	dir    string
	shards sync.Map // project -> *shardSlot
	// flushScheduled 去重防抖落盘：第一次写入时排一次 500ms 后的 Flush，
	// 期间再多的写入不再重复排。用 CAS 表达「只有一个人排」，不需要锁。
	flushScheduled atomic.Bool
}

// Open 打开权重存储。dir 为空时退化为纯内存状态（与旧版 path=="" 同语义）。
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if dir == "" {
		return s, nil
	}
	// 目录不存在不算错误：第一次启动还没有任何读取记录。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return s, nil
}

// loadShard 从磁盘读一个项目的分片。文件不存在返回空 shard。
//
// 读盘发生在**没有任何共享状态被保护**的情况下，且同一项目只读一次。
func (s *Store) loadShard(project string) (*shard, error) {
	fresh := &shardSlot{done: make(chan struct{})}
	v, loaded := s.shards.LoadOrStore(project, fresh)
	sl := v.(*shardSlot)
	if loaded {
		<-sl.done
		return sl.sh, sl.err
	}

	sh := &shard{Version: formatVersion}
	if s.dir != "" {
		var doc struct {
			Version int              `json:"version"`
			Entries map[string]entry `json:"entries"`
		}
		data, err := os.ReadFile(shardFile(s.dir, project))
		if err == nil {
			_ = json.Unmarshal(data, &doc)
			for k, e := range doc.Entries {
				sh.Entries.Store(k, e)
			}
		} else if !os.IsNotExist(err) {
			sl.err = err
			close(sl.done)
			s.shards.CompareAndDelete(project, fresh)
			return nil, err
		}
		sh.Version = formatVersion
	}
	sl.sh = sh
	close(sl.done)
	return sh, nil
}

// rangeShards 遍历所有**已加载完成**的分片。
func (s *Store) rangeShards(fn func(project string, sh *shard)) {
	s.shards.Range(func(k, v any) bool {
		sl, ok := v.(*shardSlot)
		if !ok {
			return true
		}
		select {
		case <-sl.done:
			if sl.sh != nil {
				fn(k.(string), sl.sh)
			}
		default:
			// 还在加载，跳过。
		}
		return true
	})
}

// getEntry 读一条记录。
func getEntry(sh *shard, addr string) (entry, bool) {
	v, ok := sh.Entries.Load(addr)
	if !ok {
		return entry{}, false
	}
	e, _ := v.(entry)
	return e, true
}

// projectOfAddr 从规范键里取项目名。键必须是 project@branch:path（或旧 project@branch/path）。
// 无法解析时返回 bareProject——这种键依然可以记账（落到 bare 分片），只是不能分片同步。
const bareProject = "_bare"

func projectOfAddr(addr string) string {
	at := strings.Index(addr, "@")
	if at <= 0 {
		return bareProject
	}
	return addr[:at]
}

// Count 返回一条记忆的累计读取数。
func (s *Store) Count(addr string) int {
	sh, err := s.loadShard(projectOfAddr(addr))
	if err != nil || sh == nil {
		return 0
	}
	e, _ := getEntry(sh, addr)
	return e.Hits
}

// LastRead 返回最后读取时间；从未读过返回 0。
func (s *Store) LastRead(addr string) int64 {
	sh, err := s.loadShard(projectOfAddr(addr))
	if err != nil || sh == nil {
		return 0
	}
	e, _ := getEntry(sh, addr)
	return e.Last
}

// Bump 累加一次读取。返回累加后的计数。
func (s *Store) Bump(addr string, now int64) int {
	sh, err := s.loadShard(projectOfAddr(addr))
	if err != nil || sh == nil {
		return 0
	}
	var hits int
	for {
		cur, ok := getEntry(sh, addr)
		next := cur
		next.Hits++
		if now > next.Last {
			next.Last = now
		}
		var swapped bool
		if ok {
			swapped = sh.Entries.CompareAndSwap(addr, cur, next)
		} else {
			_, loaded := sh.Entries.LoadOrStore(addr, next)
			swapped = !loaded
		}
		if swapped {
			hits = next.Hits
			break
		}
	}
	sh.dirty.Store(true)
	s.scheduleFlush()
	return hits
}

// Merge 把另一份同结构数据合并进来（对等同步用）。
// 语义见包注释：hits 取 max，last 取 max。
// 返回发生变化的键数。
func (s *Store) Merge(project string, other map[string]entry) (int, error) {
	sh, err := s.loadShard(project)
	if err != nil {
		return 0, err
	}
	changed := 0
	for addr, e := range other {
		for {
			cur, ok := getEntry(sh, addr)
			newE := e
			if cur.Hits > newE.Hits {
				newE.Hits = cur.Hits
			}
			if cur.Last > newE.Last {
				newE.Last = cur.Last
			}
			if newE == cur {
				break
			}
			var swapped bool
			if ok {
				swapped = sh.Entries.CompareAndSwap(addr, cur, newE)
			} else {
				_, loaded := sh.Entries.LoadOrStore(addr, newE)
				swapped = !loaded
			}
			if swapped {
				changed++
				break
			}
		}
	}
	if changed > 0 {
		sh.dirty.Store(true)
		s.scheduleFlush()
	}
	return changed, nil
}

// addMerge 是迁移专用的合并：hits 相加，last 取 max。
//
// 为什么迁移用 sum 而同步用 max：
//   - 同步：两台机器各自记录的计数是同一个时段的平行观测，取 max 避免重复膨胀。
//   - 迁移：同一个记忆在项目改名前后的读取是不同时段的事件，10 次 + 5 次
//     就是它被读了 15 次，必须相加，否则改名等于凭空抹掉旧热度。
func (s *Store) addMerge(project string, other map[string]entry) (int, error) {
	sh, err := s.loadShard(project)
	if err != nil {
		return 0, err
	}
	changed := 0
	for addr, e := range other {
		for {
			cur, ok := getEntry(sh, addr)
			next := cur
			next.Hits += e.Hits
			if e.Last > next.Last {
				next.Last = e.Last
			}
			var swapped bool
			if ok {
				swapped = sh.Entries.CompareAndSwap(addr, cur, next)
			} else {
				_, loaded := sh.Entries.LoadOrStore(addr, next)
				swapped = !loaded
			}
			if swapped {
				changed++
				break
			}
		}
	}
	if changed > 0 {
		sh.dirty.Store(true)
		s.scheduleFlush()
	}
	return changed, nil
}

// Export 导出一个项目的全部条目（同步的对端）。
func (s *Store) Export(project string) map[string]entry {
	sh, err := s.loadShard(project)
	if err != nil || sh == nil {
		return nil
	}
	out := map[string]entry{}
	sh.Entries.Range(func(k, v any) bool {
		if e, ok := v.(entry); ok {
			out[k.(string)] = e
		}
		return true
	})
	return out
}

func (s *Store) scheduleFlush() {
	if s.dir == "" {
		return
	}
	// CAS 去重：500ms 窗口内只有第一个人排这次落盘。
	if !s.flushScheduled.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(500*time.Millisecond, func() {
		s.flushScheduled.Store(false)
		_ = s.Flush()
	})
}

// Flush 把所有脏分片写盘。每个分片独立原子写（tmp + rename），
// 单个项目的写入不再牵动其他项目。
func (s *Store) Flush() error {
	if s.dir == "" {
		return nil
	}
	var firstErr error
	s.rangeShards(func(project string, sh *shard) {
		if !sh.dirty.Load() {
			return
		}
		if err := flushShard(s.dir, project, sh); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		sh.dirty.Store(false)
	})
	return firstErr
}

func flushShard(dir, project string, sh *shard) error {
	entries := map[string]entry{}
	sh.Entries.Range(func(k, v any) bool {
		if e, ok := v.(entry); ok {
			entries[k.(string)] = e
		}
		return true
	})
	doc := struct {
		Version int              `json:"version"`
		Entries map[string]entry `json:"entries"`
	}{Version: sh.Version, Entries: entries}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	path := shardFile(dir, project)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Prune 清理索引里已不存在的键（记忆被删后权重不应残留）。
// exists 只会收到规范键。返回剔除的键数。
func (s *Store) Prune(exists func(addr string) bool) int {
	removed := 0
	s.rangeShards(func(_ string, sh *shard) {
		var gone []any
		sh.Entries.Range(func(k, _ any) bool {
			if !exists(k.(string)) {
				gone = append(gone, k)
			}
			return true
		})
		for _, k := range gone {
			sh.Entries.Delete(k)
			removed++
		}
		if len(gone) > 0 {
			sh.dirty.Store(true)
		}
	})
	s.scheduleFlush()
	return removed
}

// Projects 返回磁盘上所有有记录的项目名（同步用）。
//
// 它扫描目录而不是只列内存里已加载的分片：分片是按需加载的，
// 如果只看 s.shards，刚启动、什么都没读时就会返回空，
// 同步会误以为「本机没有任何权重」。
func (s *Store) Projects() []string {
	seen := map[string]bool{}
	s.shards.Range(func(k, _ any) bool {
		seen[k.(string)] = true
		return true
	})
	if s.dir != "" {
		_ = filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
				return nil
			}
			rel, rerr := filepath.Rel(s.dir, path)
			if rerr != nil {
				return nil
			}
			proj := strings.TrimSuffix(filepath.ToSlash(rel), ".json")
			seen[proj] = true
			return nil
		})
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Close 停止防抖计时器并落盘。
func (s *Store) Close() error {
	s.flushScheduled.Store(true) // 阻止新的防抖排程
	return s.Flush()
}

// legacyBranchRemap 历史分支名 → 当下分支名。
//
// 记忆库在 2026-11 初做过一次分支大扫底：main/record/thought/review/fact/me
// 等散乱分支合并成 main + main.record 两根。旧权重里的分支名已经不存在，
// 不重定向的话这些热度永远对不上索引键。
//
// 数据去向（当时迁移脚本的记录）：
//   record/review/thought/fact → main.record（事实性内容）
//   me/chat/private/dev/zero/src/fact-weft → 已废弃或并入对应项目，
//     这些分支上的读取记录对应的文件大多已不在，保留原键也无妨——
//     Prune 会在下次重启时清掉索引里不存在的键。
var legacyBranchRemap = map[string]string{
	"record":  "main.record",
	"review":  "main.record",
	"thought": "main.record",
	"fact":    "main.record",
}

// legacyBranchPathPrefix 旧分支合入 main.record 时路径保留的前缀。
// 当时迁移把 review/ 分支的文件放到 main.record:review/ 下，
// thought/record 同理（目录名即原分支名），所以旧键
// zero-agi@review/2026-11-03/x.md 的当下地址是
// BlackEggAI/zero@main.record:review/2026-11-03/x.md。
var legacyBranchPathPrefix = map[string]string{
	"review":  "review/",
	"thought": "thought/",
	"fact":    "fact/",
}

// normalizeLegacyAddr 把历史遗留键映射到当下的规范键：
//   - zero-agi@... / dev/zero@... / zero@... -> BlackEggAI/zero@...
//   - dev/weft@... / weft@...               -> BlackEggAI/weft@...
//   - project@branch/path                   -> project@branch:path
//   - record/review/thought/fact 分支       -> main.record
func normalizeLegacyAddr(addr string) string {
	// 先改前缀
	for _, pfx := range []struct{ old, new string }{
		{"zero-agi@", "BlackEggAI/zero@"},
		{"dev/zero@", "BlackEggAI/zero@"},
		{"zero@", "BlackEggAI/zero@"},
		{"dev/weft@", "BlackEggAI/weft@"},
		{"weft@", "BlackEggAI/weft@"},
	} {
		if strings.HasPrefix(addr, pfx.old) {
			addr = pfx.new + strings.TrimPrefix(addr, pfx.old)
			break
		}
	}
	// 再修分支与路径的分隔符：第一个 @ 之后的第一个 / 如果没出现过 :，转成 :
	at := strings.Index(addr, "@")
	if at > 0 {
		rest := addr[at+1:]
		branch := rest
		path := ""
		if idx := strings.Index(rest, ":"); idx >= 0 {
			branch, path = rest[:idx], rest[idx:]
		} else if slash := strings.Index(rest, "/"); slash > 0 {
			branch, path = rest[:slash], ":"+rest[slash+1:]
		}
		if renamed, ok := legacyBranchRemap[branch]; ok {
			// 旧分支合入 main.record 时路径带了原分支名前缀；
			// 如果旧键的路径没有这个前缀（如旧 review 分支的根级文件），补上。
			// 注意必须在 branch 被改名之前查前缀表。
			if prefix, hasPrefix := legacyBranchPathPrefix[branch]; hasPrefix {
				p := strings.TrimPrefix(path, ":")
				if p != "" && !strings.HasPrefix(p, prefix) {
					path = ":" + prefix + p
				}
			}
			branch = renamed
		}
		addr = addr[:at+1] + branch + path
	}
	return addr
}

// MigrateLegacy 从旧版单文件 weights.json 迁移。
//
// 旧格式 {"hits":{...},"last":{...}} 的键有两种性质：
//
//  1. 带 @ 的规范键（project@branch:path 或旧语法 project@branch/path）
//     —— 这是真实的读取记录，迁移时规范化后相加。
//  2. 不带 @ 的裸键（project/path）—— 旧版双写留下的重复半：
//     每次 read 同时 Bump 规范键和裸键，两者计数永远相等。
//     它们是同一份数据的副本，迁移时直接丢弃（保留的话会与原键重复）。
//
// 规范化规则：
//   - zero-agi@ / dev/zero@ / zero@ -> BlackEggAI/zero@
//   - dev/weft@ / weft@             -> BlackEggAI/weft@
//   - project@branch/path           -> project@branch:path
//
// 迁移用 sum 而非 max（见 addMerge 注释）：项目改名前后是同一条记忆的
// 不同 Era，10 次 + 5 次就是 15 次。迁移完成后旧文件改名为 .migrated，
// 保证只跑一次（sum 不幂等，靠「跑完就退役」保证正确性）。
//
// 返回（迁移的键数，丢弃的裸键数，错误）。
func MigrateLegacy(legacyPath, dir string) (migrated, dropped int, err error) {
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	var legacy struct {
		Hits map[string]int   `json:"hits"`
		Last map[string]int64 `json:"last"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return 0, 0, fmt.Errorf("parse legacy weights: %w", err)
	}

	store, err := Open(dir)
	if err != nil {
		return 0, 0, err
	}
	defer store.Close()

	grouped := map[string]map[string]entry{}
	for rawAddr, hits := range legacy.Hits {
		if !strings.Contains(rawAddr, "@") {
			// 裸键：双写副本，丢弃
			dropped++
			continue
		}
		normAddr := normalizeLegacyAddr(rawAddr)
		proj := projectOfAddr(normAddr)
		if proj == bareProject {
			dropped++
			continue
		}
		if grouped[proj] == nil {
			grouped[proj] = map[string]entry{}
		}
		e := grouped[proj][normAddr]
		e.Hits += hits
		if l := legacy.Last[rawAddr]; l > e.Last {
			e.Last = l
		}
		grouped[proj][normAddr] = e
		migrated++
	}

	for proj, entries := range grouped {
		if _, err := store.addMerge(proj, entries); err != nil {
			return migrated, dropped, err
		}
	}

	if err := store.Flush(); err != nil {
		return migrated, dropped, err
	}
	// 迁移成功：旧文件改名退役（不删除，留档）
	return migrated, dropped, os.Rename(legacyPath, legacyPath+".migrated")
}
