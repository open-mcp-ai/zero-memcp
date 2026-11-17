package gitstore

import (
	"encoding/json"
	"strings"
	"path/filepath"
	"sort"
	"time"
)

// checkpoint 是「某个 commit 时刻，分支上每个文件的最后改动时间」。
//
// # 它解决什么
//
// fileTimes() 要给每个现存路径定「最后一次改动它的提交时间」，做法是
// 从 tip 沿父链往回走、逐段 diffTree，直到所有路径都有着落。代价正比于
// **commit 深度**，不是文件数。实测：
//
//	main.chat    1,506 commit   3,042 文件   List() 8.29s
//	main.record    121 commit   4,953 文件   List() 0.71s
//
// 而 main.chat 里绝大多数 commit 是 `mem: ... batch of 1` 这种噪音。
//
// # 为什么是「带数据的锚点」而不是纯 tag
//
// 纯 tag 只是指向某个 commit 的指针。走到它时仍然不知道更早的历史里
// 每个文件是什么时候改的——还是要继续往回走。真正能截断遍历的，
// 是「在这个 commit 上，每个路径的时间已经是多少」这份数据。
//
// 所以 checkpoint = tag（定位到 commit）+ 一份 JSON（每个路径的时间）。
// 走到 checkpoint 的 commit 时，把这份数据并进来，剩下的路径继续走，
// 已经有的直接停。
//
// # 不变式
//
// checkpoint 里的时间对它记录的那个 commit **永久成立**：时间只由
// 更早的历史决定，而历史不会被改写（这里不重写历史）。所以它一旦
// 写下就永远有效，不需要失效机制。
type checkpoint struct {
	// Commit 是这份数据对应的 commit SHA。
	Commit string `json:"commit"`
	// When 是记录时间（诊断用）。
	When time.Time `json:"when"`
	// Times 是 path -> 最后改动它的提交时间（Unix 秒）。
	Times map[string]int64 `json:"times"`
}

// checkpointTagPrefix 是 tag 名前缀。用 tag 而不是普通文件，是因为
// tag 本身就在 git 里，会随 push 一起走——换一台机器照样能受益。
const checkpointTagPrefix = "zmcp/"

func (r *Repo) checkpointTagName(ref string) string {
	return checkpointTagPrefix + normalizeRef(ref)
}

func (r *Repo) checkpointPath(ref string) string {
	return filepath.Join(r.Path, "zmcp", "checkpoints", normalizeRef(ref)+".json")
}

// ReadCheckpoint 读某条分支的 checkpoint。没有就返回 nil（不是错误）。
func (r *Repo) ReadCheckpoint(ref string) (*checkpoint, error) {
	data, err := r.FS().ReadFile(r.checkpointPath(ref))
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		// 坏掉的 checkpoint 当作没有：它只是加速，不是真相。
		return nil, nil
	}
	if cp.Commit == "" || len(cp.Times) == 0 {
		return nil, nil
	}
	// 校验：它记录的那个 commit 必须还在。否则这份数据没有意义。
	if _, _, err := r.readObject(cp.Commit); err != nil {
		return nil, nil
	}
	return &cp, nil
}

// WriteCheckpoint 记录「在 commit sha 这一刻，各路径的最后改动时间」。
//
// 同时打一个 tag 指向那个 commit：tag 是 git 里的一等公民，
// 会随 push 到远端，换机器也能受益。JSON 是数据本身。
func (r *Repo) WriteCheckpoint(ref, sha string, times map[string]int64) error {
	if sha == "" || len(times) == 0 {
		return nil
	}
	cp := checkpoint{Commit: sha, When: time.Now(), Times: times}
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	if err := r.FS().WriteAtomic(r.checkpointPath(ref), data, 0o644); err != nil {
		return err
	}
	// tag 失败不算失败：数据已经落盘，tag 只是让远端也能看到锚点。
	_ = r.writeTag(r.checkpointTagName(ref), sha)
	return nil
}

// writeTag 写一个轻量 tag（refs/tags/<name> -> sha）。
func (r *Repo) writeTag(name, sha string) error {
	if err := validSHA(sha); err != nil {
		return err
	}
	path := filepath.Join(r.Path, "refs", "tags", filepath.FromSlash(name))
	if err := r.FS().MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return r.FS().WriteAtomic(path, []byte(sha+"\n"), 0o644)
}

// CheckpointTimes 返回 checkpoint 里「目标 commit 之下的时间」。
//
// only 里的路径才会被采纳（其余路径调用方已经知道了，不必占内存）。
func (cp *checkpoint) pick(only map[string]bool, out map[string]int64) int {
	if cp == nil {
		return 0
	}
	n := 0
	for p, t := range cp.Times {
		if _, had := out[p]; had {
			continue
		}
		if only != nil && !only[p] {
			continue
		}
		out[p] = t
		n++
	}
	return n
}

// sortedPaths 把 map 的键排序（写 checkpoint 时让输出稳定）。
func sortedPaths(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// logCheckpointFailure 记一次锚点写入失败（只是加速失效，不影响正确性）。
func (r *Repo) logCheckpointFailure(ref string, err error) {
	// 有意静默：锚点写不进去只意味着下次遍历慢一点，不是错误。
	_, _ = ref, err
}

// TagObject 是一个已解析的 annotated tag。
type TagObject struct {
	// Target 是它指向的对象 SHA。
	Target string
	// Body 是 tag 对象的原始正文（含头部与摘要）。
	Body string
}

// ReadTagObject 读一个 annotated tag。
//
// 返回 ok=false 表示这不是 annotated tag（轻量 tag 只是一个 ref 文件，
// 没有 tag 对象）。读取失败返回 error。
func (r *Repo) ReadTagObject(name string) (TagObject, bool, error) {
	path := filepath.Join(r.Path, "refs", "tags", filepath.FromSlash(name))
	raw, err := r.FS().ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return TagObject{}, false, nil
		}
		return TagObject{}, false, err
	}
	sha := strings.TrimSpace(string(raw))
	if validSHA(sha) != nil {
		return TagObject{}, false, nil
	}
	typ, body, err := r.readObject(sha)
	if err != nil {
		return TagObject{}, false, err
	}
	if typ != "tag" {
		// 轻量 tag：ref 直接指向 commit。
		return TagObject{}, false, nil
	}
	obj := TagObject{Body: string(body)}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "object ") {
			obj.Target = strings.TrimSpace(strings.TrimPrefix(line, "object "))
			break
		}
		if line == "" {
			break // 头部结束
		}
	}
	return obj, true, nil
}

// DailyCheckpointTag 返回某条分支某天的锚点 tag 名。
//
//	zmcp/<branch>/<YYYY-MM-DD>
//
// 用 refs/tags/ 而不是私有命名空间，是因为 tag 会随 push 到远端——
// 换一台机器、从备份恢复，都不用重新走一遍父链。
func DailyCheckpointTag(ref, date string) string {
	return "zmcp/" + normalizeRef(ref) + "/" + date
}

// WriteDailyCheckpoint 在 tip 上打一个当天的锚点 tag。
//
// tag 正文 = 摘要 + 一行指向数据 blob 的指针：
//
//	今天的摘要（人或 AI 写的，可以为空）
//
//	zmcp-checkpoint: <blob-sha>
//
// 为什么数据放 blob 而不是塞进 tag 正文：main.record 有 4,953 条路径，
// 序列化后约 150KB。塞进正文会让 `git show <tag>` 吐出一大坨机器数据；
// 放 blob 则正文保持人类可读，git 还替我们压缩。
func (r *Repo) WriteDailyCheckpoint(ref, date, summary string, times map[string]int64) error {
	normRef := normalizeRef(ref)
	sha, err := r.Resolve(normRef)
	if err != nil || sha == "" {
		return err
	}
	if len(times) == 0 {
		return nil
	}
	cp := checkpoint{Commit: sha, When: time.Now(), Times: times}
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	blobSHA, err := r.writeObject("blob", data)
	if err != nil {
		return err
	}

	// 清理旧格式残留：早先锚点是 zmcp/<branch>（一个 ref 文件），
	// 现在是 zmcp/<branch>/<date>（目录）。前者存在时后者 mkdir 会失败
	// （not a directory）。zmcp/ 是我们自己的命名空间，删掉是安全的。
	legacy := filepath.Join(r.Path, "refs", "tags", "zmcp", filepath.FromSlash(normRef))
	_ = r.FS().Remove(legacy)

	name := DailyCheckpointTag(normRef, date)
	body := summary
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += "\nzmcp-checkpoint: " + blobSHA + "\n"

	tagSHA, err := r.writeTagObject(name, sha, body, time.Now())
	if err != nil {
		return err
	}
	if err := r.updateRef("refs/tags/"+name, tagSHA); err != nil {
		return err
	}
	// 顺手更新「最新锚点」缓存，让下一次 fileTimes 不必扫 tag。
	_ = r.FS().WriteAtomic(r.checkpointPath(normRef), data, 0o644)
	return nil
}

// LatestDailyCheckpoint 找某条分支最近的一个锚点 tag，返回它的数据。
//
// 找不到返回 nil（不是错误）：第一次跑的时候本来就没有锚点。
func (r *Repo) LatestDailyCheckpoint(ref string) (*checkpoint, error) {
	normRef := normalizeRef(ref)
	prefix := "zmcp/" + normRef + "/"

	// refs/tags 下找 zmcp/<branch>/<date>，取字典序最大的（日期天然可比）
	tagsDir := filepath.Join(r.Path, "refs", "tags", "zmcp", filepath.FromSlash(normRef))
	entries, err := r.FS().ReadDir(tagsDir)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	latest := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name > latest {
			latest = name
		}
	}
	if latest == "" {
		return nil, nil
	}

	obj, ok, err := r.ReadTagObject(prefix + latest)
	if err != nil || !ok {
		return nil, err
	}
	// 从正文里找 zmcp-checkpoint: <sha>
	var blobSHA string
	for _, line := range strings.Split(obj.Body, "\n") {
		if v, found := strings.CutPrefix(line, "zmcp-checkpoint: "); found {
			blobSHA = strings.TrimSpace(v)
			break
		}
	}
	if blobSHA == "" || validSHA(blobSHA) != nil {
		return nil, nil
	}
	typ, body, err := r.readObject(blobSHA)
	if err != nil || typ != "blob" {
		return nil, nil
	}
	var cp checkpoint
	if err := json.Unmarshal(body, &cp); err != nil {
		return nil, nil
	}
	if cp.Commit == "" || len(cp.Times) == 0 {
		return nil, nil
	}
	return &cp, nil
}


// HasDailyCheckpoint 报告某条分支今天（或指定的日期）是否已有锚点 tag。
func (r *Repo) HasDailyCheckpoint(ref, date string) bool {
	path := filepath.Join(r.Path, "refs", "tags",
		filepath.FromSlash(DailyCheckpointTag(ref, date)))
	if _, err := r.FS().ReadFile(path); err != nil {
		return false
	}
	return true
}
