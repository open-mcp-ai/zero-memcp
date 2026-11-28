package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"os"
	"path/filepath"

	"github.com/open-mcp-ai/zero-memcp/internal/index"
)

// 索引必须配置才生效。没有配置 [index] 时作为冷备份节点运行：
// 1. 启动不扫描、不建索引、不预热正文缓存（内存极小、启动极快）
// 2. status 明确声明 index: disabled (cold backup)
// 3. recall / grep 调用明确报错拒收，禁止静默返回空结果（静默失败是最贵的故障）
// 4. Git 写入与命令流落地完全不受影响（Git 是真相源）
func TestIndexDisabledByDefault(t *testing.T) {
	// 1) 解析不带 [index] 的配置
	cfg, err := config.ParseFile([]byte(`
[server]
data = "/tmp/zm-cold-test"

[[user]]
name = "zero"
root = true
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Index.Enabled {
		t.Fatalf("没配 [index] 时默认不应启用索引（冷备份安全默认）")
	}

	// 2) 显式配 enabled = true
	cfgEnabled, err := config.ParseFile([]byte(`
[server]
data = "/tmp/zm-cold-test"

[[user]]
name = "zero"
root = true

[index]
enabled = true
`))
	if err != nil {
		t.Fatalf("parse enabled: %v", err)
	}
	if !cfgEnabled.Index.Enabled {
		t.Fatalf("配了 [index] enabled = true 时应启用索引")
	}

	// 3) 显式配 enabled = false
	cfgDisabled, err := config.ParseFile([]byte(`
[server]
data = "/tmp/zm-cold-test"

[[user]]
name = "zero"
root = true

[index]
enabled = false
`))
	if err != nil {
		t.Fatalf("parse disabled: %v", err)
	}
	if cfgDisabled.Index.Enabled {
		t.Fatalf("配了 [index] enabled = false 时不应启用索引")
	}
}

// 索引关闭时，落到本机的每一条路径都不许再进索引。
//
// 踩过的坑（2026-11-24 实测，rock64）：启动路径上拦住了索引，
// 但 refreshPath 没有守卫——接收命令流时逐条路径建索引，
// 容器内存从 3.97 MiB 涨到 1.547 GiB，CPU 95%，
// 而 status 上明明白白写着 "index: disabled"。
// 「配置说不建、代码还在建」比完全不建更坏：人以为省了。
func TestIndexDisabledSkipsRefreshPath(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	srv.indexEnabled = false
	// 换一个空索引，确保后续计数只反映新增
	srv.treesPtr.Store(&treeSet{trees: nil, idx: index.New()})

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 写一篇记忆：Git 必须落盘，索引必须一动不动
	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:cold.md $BODY",
			"vars": map[string]string{"BODY": "冷备份也要落盘"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 内容必须真的进了 Git（冷备份的唯一职责）
	repo, err := srv.projects.Get("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := repo.Stat("zero", "cold.md"); !found {
		t.Fatalf("索引关闭不影响落盘：Git 里必须有 cold.md")
	}

	// 索引必须仍然是空的
	if got := srv.idxNow().Docs(); got != 0 {
		t.Fatalf("索引已关闭，不该有任何文档进索引，实际 %d 篇——"+
			"refreshPath 漏了守卫，会让冷备份节点悄悄建回全量索引", got)
	}
}

// 分支头移动是外部推进的常态路径，冷备份节点也不许在这里建索引。
//
// 踩过的坑（2026-11-24，rock64 实测 8 小时）：
// 启动时老实跳过索引，300 秒后一次 head movement 走 refreshBranches →
// refreshBranch → scanGitProject 全量重扫，17 分钟建出 26,502 篇索引，
// 然后每 30 秒存一次 61 MB 快照，烧掉 359 分钟 CPU、常驻 1.6 GiB，
// 而 status 上始终写着 "index: disabled"。只拦启动路径是不够的。
// 外部提交与磁盘快照双重守卫：冷备份节点既不许读旧快照，也不许在提交时建索引。
//
// 踩过的坑（2026-11-24，rock64 实测 8 小时 359 分钟 CPU）：
// 历史上建过索引，磁盘上残留了快照文件。新版本启动虽然跳过了全量扫描，
// 但 tryLoadIndexSnapshot 却把这几万篇文档读了回来挂上，维护线程随后把
// 这批树当活物监视；外部一有提交就触发全量重扫，随后每 30 秒存一次 61 MB 快照，
// 常驻 1.6 GiB 内存，而 status 上始终写着 "index: disabled"。
func TestIndexDisabledSkipsBranchRefresh(t *testing.T) {
	// 第一阶段：用一个【开启了索引】的实例制造真实的磁盘快照
	dataDir := t.TempDir()
	cfgWithIndex := `
[server]
data = ` + tomlPath(dataDir) + `

[index]
enabled = true

[[user]]
name = "zero"
root = true

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwxr-xr-x"
`
	// 用原生 os.WriteFile 而不是测试辅助 osWrite：
	// 后者会自动追加 [index] enabled = true（让老测试保持开启），
	// 在这里会把「冷备份配置」偷偷改回开启，测试就假通过了。
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfgWithIndex), 0600); err != nil {
		t.Fatal(err)
	}
	s1, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 写一篇文档并存下快照（先走一次写入把仓库创建出来）
	tok1 := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	if _, err := callMCPRaw(t, s1, tok1, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:doc.md $BODY",
			"vars": map[string]string{"BODY": "真实文档"},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	repo1, err := s1.projects.Get("blackegg/me")
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	if _, err := repo1.Commit("zero", "init"); err != nil {
		t.Fatal(err)
	}
	if err := s1.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := s1.saveIndexSnapshot(); err != nil {
		t.Fatalf("生成测试快照: %v", err)
	}
	s1.Close()

	// 确认快照确实落盘了
	if _, serr := os.Stat(s1.indexSnapshotDir()); serr != nil {
		t.Fatalf("前提不成立：快照文件没落盘: %v", serr)
	}

	// 第二阶段：在同一个 dataDir 上启动【冷备份】实例（未配置 [index]）
	cfgCold := `
[server]
data = ` + tomlPath(dataDir) + `

[[user]]
name = "zero"
root = true

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwxr-xr-x"
`
	if err := os.WriteFile(filepath.Join(dataDir, "zero-memcp.toml"), []byte(cfgCold), 0600); err != nil {
		t.Fatal(err)
	}
	s2, err := New(Config{
		DataDir:     dataDir,
		ConfigFile:  filepath.Join(dataDir, "zero-memcp.toml"),
		Listen:      "127.0.0.1:0",
		EnforceAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })

	// 1. 启动后：磁盘快照绝不许被读回来
	if got := s2.idxNow().Docs(); got != 0 {
		t.Fatalf("冷备份实例启动时误读了磁盘上的旧快照：实际读了 %d 篇——"+
			"这是 rock64 8 小时 1.6GB 的直接根因", got)
	}

	// 2. 外部推进后：绝不许扫描并建索引。
	//
	// 这里直接打 refreshBranch——它是「外部 head 移动」的落点，
	// 也是 rock64 上真正烧掉 8 小时 CPU 的那条路径。
	repo2, err := s2.projects.Get("blackegg/me")
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	if err := repo2.Write("zero", "doc2.md", []byte("第二篇"), gitstore.ModeContent); err != nil {
		t.Fatal(err)
	}
	if _, err := repo2.Commit("zero", "push"); err != nil {
		t.Fatal(err)
	}
	if err := s2.refreshBranch("blackegg/me", "zero"); err != nil {
		t.Fatalf("索引关闭时 refreshBranch 应静默跳过: %v", err)
	}
	if got := s2.idxNow().Docs(); got != 0 {
		t.Fatalf("refreshBranch 在冷备份节点误建了索引：实际 %d 篇", got)
	}
	if got := len(s2.treeList()); got != 0 {
		t.Fatalf("refreshBranch 在冷备份节点误扫了内存树：实际 %d 棵", got)
	}

	// 3. 快照保存器绝不写盘
	if err := s2.saveIndexSnapshot(); err != nil {
		t.Fatalf("saveIndexSnapshot 应静默跳过: %v", err)
	}
}

// 验证未启用索引的实例上，调用 recall / grep 必须明确报错，禁止静默返回空
func TestColdBackupRefusesQueryVerbsLoudly(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	// 关闭索引
	srv.indexEnabled = false

	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// grep 必须报错
	_, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd": "grep something",
		},
	})
	if err == nil {
		t.Fatalf("未启用索引时 grep 必须报错出声，禁止静默返回空")
	}
	if !strings.Contains(err.Error(), "index is disabled") {
		t.Fatalf("报错信息必须说明原因（index is disabled），实际: %v", err)
	}

	// recall 必须报错
	_, err = callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd": "recall something",
		},
	})
	if err == nil {
		t.Fatalf("未启用索引时 recall 必须报错出声")
	}
	if !strings.Contains(err.Error(), "index is disabled") {
		t.Fatalf("报错信息必须说明原因，实际: %v", err)
	}

	// status 必须明确打印 index: disabled
	txt, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd": "status",
		},
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(txt, "index: disabled") {
		t.Fatalf("status 必须显示 index: disabled，实际:\n%s", txt)
	}
}
