package server

// knownVerbs 是模板的动词全集，只用于「拼错时提一句」。
//
// 它必须与 server.go 的分发开关保持一致：多写一个不存在的动词，
// 会把用户引向一个必然失败的调用；少写一个，则拼错时得不到提示。
var knownVerbs = []string{
	"help", "recall", "read", "grep", "log", "ls", "put", "trees", "remind", "status",
	"commit", "diff", "batch", "branch", "tag", "cp", "mv", "delete", "chmod",
	"lock", "unlock", "archive", "reset", "write", "config", "reload", "whoami",
	"users", "since", "fetch", "remote", "remote-key", "remote-secret", "push", "sync",
}

// suggestVerb 对未知动词做一次「一个编辑距离」的提示。
//
// 为什么只做提示、不做自动纠正：recal 与 recall 差一个字母，
// 但 msg 与 msq、ls 与 l 之间也可能只差一个字母而语义完全不同。
// 猜错的代价是执行了一个用户没要求的操作——比报错贵得多。
func suggestVerb(verb string) string {
	best := ""
	bestDist := 2 // 只接受距离 1
	for _, k := range knownVerbs {
		if d := editDistance(verb, k); d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}

// editDistance 是标准 Levenshtein 距离。
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := 0; j <= len(rb); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
