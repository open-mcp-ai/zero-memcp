package config

import (
	"path/filepath"
	"testing"
)

// 每一个「被解析的段」都必须在 knownKeys 里登记。
//
// 踩过的坑（2026-11-21）：[search] body_cache_mb 在 ParseFile 里有解析代码，
// 但 knownKeys 漏了 "search"。结果是这个旋钮**根本无法通过配置文件设置**——
// 写了就报 "unknown section [search] (typo?)"，而不写又永远走默认值。
//
// 死旋钮比缺旋钮更坏：文档写着能配，用户照着写，得到的是一个
// 看起来像拼写错误的报错，于是去怀疑自己名字打错了。
//
// 这条测试用「段真的能解析出来」作为判据，不依赖是否记得同步更新列表。
func TestParsedSectionsAreDeclaredInKnownKeys(t *testing.T) {
	// 每个被 ParseFile 认领的段，给一份最小合法内容。
	cases := map[string]string{
		"search":   "[search]\nbody_cache_mb = 128\n",
		"erase":    "[erase]\nrequire_ticket = true\n",
		"commit":   "[commit]\ncount = 10\n",
		"gc":       "[gc]\ninterval = \"24h\"\n",
		"sync":     "[sync]\ninterval = \"2h\"\n",
		"server":   "[server]\ndata = \"~/x\"\n",
		"semantic": "[semantic]\nbatch_size = 8\n",
		"auth.key": "[[auth.key]]\nname = \"k\"\npublic_key = \"AAAA\"\n",
		"listen":   "[[listen]]\naddr = \"127.0.0.1:1729\"\nallow = [\"*\"]\n",
		"user":     "[[user]]\nname = \"extra\"\nroot = false\n",
		"group":    "[[group]]\nname = \"g\"\nmembers = [\"zero\"]\n",
		"project":  "[[project]]\nname = \"p\"\nowner = \"zero\"\nmode = \"rwx------\"\n",
		// [[access]] 引用的项目必须是已声明的，所以这里一并带上 project。
		"access": "[[access]]\nuser = \"zero\"\nproject = \"p\"\nperm = \"r\"\n\n" +
			"[[project]]\nname = \"p\"\nowner = \"zero\"\nmode = \"rwx------\"\n",
		"remote":            "[[remote]]\nproject = \"p\"\nname = \"r\"\nurl = \"http://127.0.0.1:1729\"\n",
		"semantic.provider": "[[semantic.provider]]\nlabel = \"x\"\nkind = \"http\"\nendpoint = \"http://127.0.0.1:1\"\nmodel = \"m\"\ndim = 8\n",
	}
	for section, body := range cases {
		if _, ok := knownKeys[section]; !ok {
			t.Errorf("段 [%s] 有解析路径，但没在 knownKeys 里登记——写了会报 unknown section", section)
		}
		// 用真实解析路径再验一次：登记了不等于真能加载。
		// 解析器要求至少一个 [[user]]（否则没人能持凭证），所以补一个。
		if _, err := ParseFile([]byte("[[user]]\nname = \"zero\"\nroot = true\n" + body)); err != nil {
			t.Errorf("段 [%s] 的最小配置解析失败: %v\n内容:\n%s", section, err, body)
		}
	}
}

// [search] body_cache_mb 的三种意图必须能区分：没写 / 写 0 / 写正数。
//
// 没写 → 默认预算；写 0 → 明确禁用（小盒子的选择）；写正数 → 按数走。
// 分不清「没写」与「写了 0」是这类旋钮最常见的错。
func TestBodyCacheMBDistinguishesUnsetFromZero(t *testing.T) {
	const base = "[[user]]\nname = \"zero\"\nroot = true\n[server]\ndata = \"~/x\"\n"
	unset, err := ParseFile([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	zero, err := ParseFile([]byte(base + "[search]\nbody_cache_mb = 0\n"))
	if err != nil {
		t.Fatalf("body_cache_mb = 0 应当被接受（表示禁用），实际: %v", err)
	}
	on, err := ParseFile([]byte(base + "[search]\nbody_cache_mb = 64\n"))
	if err != nil {
		t.Fatal(err)
	}

	if unset.Search.bodyCacheSet {
		t.Error("没写 [search] 时不该标记为已设置")
	}
	// 光有 [search] 段、里面没写 body_cache_mb——这是最容易漏的一种：
	// 段存在但旋钮缺失，早先会把「已设置」置真，预算读成 0（=禁用）。
	sectionOnly, err := ParseFile([]byte(base + "[search]\n"))
	if err != nil {
		t.Fatalf("空 [search] 段应当被接受，实际: %v", err)
	}
	if sectionOnly.Search.bodyCacheSet {
		t.Error("空 [search] 段不该标记为已设置")
	}
	if sectionOnly.Search.BodyCacheBudget() <= 0 {
		t.Error("空 [search] 段应当走默认预算，而不是被读成 0（禁用）")
	}
	if !zero.Search.bodyCacheSet || zero.Search.BodyCacheMB != 0 {
		t.Errorf("写了 0 应当记为「已设置且为 0」，实际 set=%v mb=%d",
			zero.Search.bodyCacheSet, zero.Search.BodyCacheMB)
	}
	if !on.Search.bodyCacheSet || on.Search.BodyCacheMB != 64 {
		t.Errorf("写了 64 应当记为 64，实际 set=%v mb=%d", on.Search.bodyCacheSet, on.Search.BodyCacheMB)
	}

	// 预算语义：未设置走默认，显式 0 表示禁用。
	if unset.Search.BodyCacheBudget() <= 0 {
		t.Error("未设置时应当有默认预算")
	}
	if zero.Search.BodyCacheBudget() != 0 {
		t.Errorf("显式 0 应当解析为禁用（预算 0），实际 %d", zero.Search.BodyCacheBudget())
	}
}

// 仓库根的示例配置必须真能加载。
//
// 示例是文档的一部分；如果它解析不了，那它教给用户的写法就是错的。
func TestExampleConfigLoads(t *testing.T) {
	path, err := filepath.Abs("../../zero-memcp.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	cf, err := Load(path)
	if err != nil {
		t.Fatalf("示例配置解析失败: %v", err)
	}
	if len(cf.Listens) == 0 {
		t.Fatal("示例应当至少声明一个 [[listen]]")
	}
	if len(cf.Users) == 0 || len(cf.Projects) == 0 {
		t.Fatal("示例应当声明用户与项目")
	}
	if cf.Search.BodyCacheMB != 512 {
		t.Fatalf("示例里的 body_cache_mb 应当生效为 512，实际 %d（set=%v）",
			cf.Search.BodyCacheMB, cf.Search.bodyCacheSet)
	}
}
