// Package server 实现 zero-memcp 的 HTTP MCP 服务端。
//
// 监听 127.0.0.1:1729。
// 绝不绑 0.0.0.0：这里面装的是身份与私有日记，不能向局域网暴露。
// 端口被占直接报错退出，不静默换端口——换端口会让插件连到一个空服务。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
	"github.com/open-mcp-ai/zero-memcp/internal/index"
	"github.com/open-mcp-ai/zero-memcp/internal/manifest"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
	"github.com/open-mcp-ai/zero-memcp/internal/weights"
)

// DefaultPort 是约定的固定端口：哈代-拉马努金数 1729。
const DefaultPort = 1729

// recallMinCoverage 是 recall 的最低查询词覆盖率。
//
// 中文按二元组分词，「量子计算机退相干」会拆出「计算」「算机」——
// 这两个词在几乎任何技术文档里都有。没有覆盖率门槛时，这类与记忆
// 完全无关的查询会拿到 8 分以上的「命中」，后果有两层：
//  1. 模型拿到一段不相关的正文，还以为是答案；
//  2. '||' 回落链彻底失效——上层总是「命中」，下层永远不会被问。
//
// 实测 24 个查询：真正相关的 12 个覆盖率全为 1.00，无关的 12 个最高
// 0.29。取 0.5 两边都有充足余量，不是拟合出来的阈值。
const recallMinCoverage = 0.5

// Config 启动配置。
type Config struct {
	// DataDir 是数据根。<data>/system/ 放密钥与吊销名单，
	// <data>/repos/<name>.git 放裸仓库。
	DataDir string

	// Semantic 控制是否启用向量语义检索。
	//
	// 零值（全部空串）等于关闭：不加载模型、不建向量索引、
	// srecall 返回明确错误。
	//
	// 新版支持 Providers 降级链（3060 GPU 优先 + 本地兜底），
	// 旧版单后端字段（LibPath/ModelPath/VocabPath）保留向前兼容。
	Semantic struct {
		LibPath   string // libonnxruntime 动态库路径
		ModelPath string // ONNX 模型路径
		VocabPath string // 词表路径
		// VectorDir 是向量缓存的落盘目录。空则用 <DataDir>/vectors。
		VectorDir string

		// Providers 降级链（来自配置或命令行）。
		Providers []config.SemanticProvider
		// BatchSize 批量大小，默认 32。
		BatchSize int
		// MaintainInterval 是后台追平的兜底周期。0 用默认 60s。
		MaintainInterval time.Duration
	}

	// ConfigFile 是配置文件路径。
	ConfigFile string

	// Listen 监听地址，缺省 "127.0.0.1:1729"。
	Listen string

	// Endpoint 本端点标识，例如 local / blackbox。
	Endpoint string

	// WeightsDir 阅读频次状态的目录。按项目分片存放
	// （<dir>/BlackEggAI/zero.json 等），不再是一个大文件。
	WeightsDir string

	// DefaultBranch 是默认分支名。
	//
	// 分支是「这份记忆的版本线」，不是「谁在用」——
	// 所以它是存储参数，不是身份参数。
	DefaultBranch string

	// EnforceAuth 即使在 127.0.0.1 也强制要求带 JWT Token。
	//
	// 除了代码里传（SDK），也可以由配置 [server] enforce_auth 打开。
	// 后者必须存在：运维的人不会改 Go 代码。早先这个开关只有 SDK 字段，
	// 于是「本机免签即 root」这条降级没法靠配置关掉——
	// 而同机任意进程不带凭证就能读走全部记忆，是记忆库里最大的洞。
	EnforceAuth bool

	// WebUI 控制这个实例是否提供管理面（在没有 [[listen]] 块时的默认端口）。
	//
	// 默认关闭：不是怕管理员麻烦，而是管理面一旦开在错误的端口上，
	// 就等于把「谁能签发凭证」交给网络。显式声明才算意图。
	WebUI bool

	// UIBootstrapPassword 是首次启动时自动生成的管理口令。
	//
	// 非空意味着「这是刚初始化的实例，口令还没被人换过」。
	// 只在 New 里赋值一次，调用方打印一次后就不再重复提供：
	// 引导口令的价值就是一次性，重复打印等于把它变成常驻秘密。
	UIBootstrapPassword string
}

// Viewer 标识调用方形态。
type Viewer struct {
	Channel string
}

// Server 是 zero-memcp 的核心实例。
type Server struct {
	cfg Config

	// dataDir 是数据根，吊销凭证时需要它定位 system/tokens。
	dataDir string

	cfgMu sync.RWMutex
	mu    sync.RWMutex
	trees []*memory.Tree
	idx   *index.Index

	// clock 告诉客户端「记忆库有更新」。
	//
	// 纯内存 atomic，不落盘：真相在仓库里。重启归零没关系，
	// 客户端看到读数变小会自己重新对齐基线。单一计数器即可——
	// 「记忆库更新过」是实例级事实，不需要按身份分裂。
	clock atomic.Int64

	// writeSeq 给写入文件名做同刻去重。
	//
	// 文件名原先只到毫秒（HHMMSS.mmm），并发写入会在同一毫秒算出同一个
	// 路径，后写的静默覆盖先写的——实测 100 条并发唯一内容只剩 88 条，
	// 且没有任何错误返回。时钟精度提高只是把概率压低，不能消除：
	// 真正需要的是「同一时刻的两次写入必须得到不同的名字」。
	// 计数器取的是「这一次写入」而非时间，所以单调且不会撞。
	writeSeq atomic.Uint64

	// semantic 是向量语义检索侧的运行状态。
	//
	// 与 idx（BM25）平行存在：两套索引各自独立重建，检索时融合。
	// 零值即「未启用」，所有方法都要容忍它。
	semantic semanticState
	// semanticMaint 管理语义索引的后台追平协程。
	semanticMaint semanticMaintainer

	// semanticDir 是向量缓存的落盘目录。
	semanticDir string

	// defaultBranch 是默认版本线。所有仓库共用它。
	//
	// 为什么是单一默认值而不是每用户一份：多用户只表示「哪些人能用
	// 哪些仓库」，不表示「每个人有自己的分支」。一台机器上跑的记忆
	// 就是一份，分支名是它的版本线，不是它的身份。
	defaultBranch string

	// weights 记录哪些块真的被读过。只统计 read，不统计 recall——
	// recall 只是列出候选，瞄一眼没读说明它没帮上忙。
	weights *weights.Store

	// projects 管理所有裸仓库（Git 存储模式）。
	projects *ProjectStore

	// tokens 负责 JWT 签发与验签。
	tokens *auth.TokenStore

	// rev 是吊销名单。
	rev *auth.Revocations

	// passwords 是 WebUI 管理口令（argon2id 摘要，落在 system/ui.passwd）。
	passwords *auth.PasswordStore
	// sessions 是 WebUI 登录态。只在内存里：重启即全部失效。
	sessions *auth.SessionStore
	// grants 是临时写授权表（system/grants/<jti>.json）。
	//
	// 它是权限判定的第三个来源：凭证层 ∩ 配置层，再并入有效授权。
	// 只能加不能减——要减用分支锁。
	grants *auth.GrantStore

	// uiLimiter 限制管理面登录失败速率（懒初始化）。
	uiLimiter *uiLoginLimiter

	// lock 是数据目录的跨进程独占锁。进程崩溃时内核自动释放。
	lock *instanceLock

	// cfgFile 是配置内容，cfgLookup 是它的运行时索引。
	cfgFile *config.File
	// cfgLookup 是配置的运行时索引，用 atomic.Pointer 持有。
	//
	// 为什么不能是普通字段：reload 会在运行中替换它，而它有 11 处
	// 无锁读取（权限判定、项目枚举、用户查询）。普通字段的并发写
	// 会撕出「一半新一半旧」的视图，那种坏死在权限判定上——
	// 一个本该被拒的请求可能读到旧表而放行。
	//
	// 原子替换 + 引用不可变：新表整个构建好才换指针，
	// 读到的永远是某一份完整的表。
	cfgLookup atomic.Pointer[config.Lookup]

	// treeWatch 记录哪些身份关注了哪些树路径。
	//
	// 通知不另存一份消息日志：树的 SHA 变化本身就是事件。
	// 关注关系是派生态，丢了最多重读一次，不写进不可变历史。
	treeWatch *TreeWatchHub

	// remotes 是 [[remote]] 的当前快照，随 reload 原子替换。
	remotes atomic.Pointer[[]config.Remote]

	// bcast 是 SSE 广播器：写记忆时扇出给监听者。
	bcast *Broadcaster

	// policies 缓存每个项目的写入策略（从 100755 的 SKILL.md 解析而来）。
	policies sync.Map // project -> manifest.Policy

	// dedup 记录「语义内容指纹 -> 已有地址」，用于写入幂等。
	//
	// 键是 kind + session + 正文 的 SHA-256，不含 created 时间戳——
	// 早先拿整份带时间戳的文件内容算 Git blob hash 做判重，同一段文字
	// 每次写出来的 created 都不同，哈希永远不相等，去重从未生效。
	//
	// 值带分支前缀，避免不同分支上的同一条互相判掉。
	dedup sync.Map // "project@branch:h8" -> path

	// lastCommits 记录上次看到的各分支 HEAD（键见 treeHeadKey），用来发现外部提交。
	lastCommits sync.Map // treeHeadKey(project, branch) -> commit hash

	// done 通知后台维护循环退出。
	done     chan struct{}
	doneOnce sync.Once
	// maintainDone 在维护 goroutine 真正退出后关闭。
	//
	// 为什么必须等它：Close 会关掉各仓库的写者（writerDead），
	// 而 maintain 可能正在 repo.Commit——那条路径不经过写入队列，
	// 会直接写 ref。不等待就关库，等于让提交和关库赛跑。
	maintainDone chan struct{}
	// startMu 保护下面两个字段：Start 写、Close 读。
	startMu         sync.Mutex
	maintainStarted bool

	// netMu 保护下面两个字段：Start 在 goroutine 里写，Addr/Close 在别处读。
	netMu       sync.Mutex
	httpServers []*http.Server
	listens     []ListenBlock
	listener    net.Listener
}

// setListener 记录监听器，供 Addr 和 Close 使用。
func (s *Server) setListener(ln net.Listener) {
	s.netMu.Lock()
	s.listener = ln
	s.netMu.Unlock()
}

// getListener 读取监听器。
func (s *Server) getListener() net.Listener {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	return s.listener
}

// New 构建并校验服务端。校验在启动前完成，不在运行中抛出。
func New(cfg Config) (*Server, error) {
	// 配置是权限与网络参数的真相源。
	// 优先读它，没有才 fallback 到 flag 和硬编码默认值。
	var cfgFile *config.File
	var lookup *config.Lookup
	if cfg.ConfigFile != "" {
		var err error
		cfgFile, err = config.Load(cfg.ConfigFile)
		if err != nil {
			return nil, err
		}
		lookup = cfgFile.Build()
		if cfg.DataDir == "" && cfgFile.Server.Data != "" {
			cfg.DataDir = cfgFile.Server.Data
		}
		// 配置的 enforce_auth 只在调用方没显式给时代入。
		if !cfg.EnforceAuth && cfgFile.Server.EnforceAuth {
			cfg.EnforceAuth = true
		}
		if cfg.Endpoint == "" {
			cfg.Endpoint = cfgFile.Server.Endpoint
		}
		// 配置里的 listen 优先于默认的 1729，但低于命令行显式给的 -listen。
		if cfgFile.Server.Listen != "" && (cfg.Listen == "" || cfg.Listen == fmt.Sprintf("127.0.0.1:%d", DefaultPort)) {
			cfg.Listen = cfgFile.Server.Listen
		}
	}

	if cfg.Listen == "" {
		cfg.Listen = fmt.Sprintf("127.0.0.1:%d", DefaultPort)
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q: %w", cfg.Listen, err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil, fmt.Errorf("refusing to bind to a non-loopback address: %q", host)
	}

	if cfg.DataDir == "" {
		return nil, errors.New("data directory required: pass -data or set [server] data in the config file")
	}

	// 数据目录独占锁。必须在读 jwt.key / ui.passwd 之前拿到：
	//
	// 两个实例同时启动时会争着生成签名密钥与管理口令，
	// 谁先落盘谁说了算，而另一个进程拿着自己刚生成的密钥继续跑——
	// 结果是同一个数据目录里存在两套互不相认的身份体系。
	// 把锁放在任何读写 system/ 之前，才能保证「首次启动」只发生一次。
	startupLock, err := acquireInstanceLock(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	startupOK := false
	defer func() {
		// 半途失败时释放锁；成功路径的所有权交给 *Server。
		// 不能每个 return 都手写一遍 unlock——漏一个就是永久死锁。
		if startupOK {
			return
		}
		_ = startupLock.Release()
	}()

	w, err := weights.Open(cfg.WeightsDir)
	if err != nil {
		return nil, fmt.Errorf("open weights state: %w", err)
	}

	// 签名密钥必须落盘。每次启动重新生成会让所有已签发的 token
	// 同时失效，而那种故障的表现是「所有设备一起 401」——
	// 排查的人会先怀疑网络和配置，最后才想到密钥。
	var tokens *auth.TokenStore
	var rev *auth.Revocations
	var pwStore *auth.PasswordStore
	if cfg.DataDir != "" {
		systemDir := filepath.Join(cfg.DataDir, "system")
		secret, err := auth.LoadOrCreateSecret(filepath.Join(systemDir, "jwt.key"))
		if err != nil {
			return nil, fmt.Errorf("load signing key: %w", err)
		}
		tokens = auth.NewTokenStore(secret)
		rev = auth.NewRevocations(filepath.Join(systemDir, "revoked"))

		// The password store is initialized here, but bootstrap is deferred until
		// the very end of New. If index loading fails after generating a random
		// password, the caller cannot print it and the next start cannot recover
		// it. Bootstrap only after all other fallible initialization succeeds.
		pwStore = auth.NewPasswordStore(systemDir)
	} else {
		tokens = auth.NewTokenStore(nil)
	}

	// 默认分支在启动时定一次，而且必须在 Reload 之前：
	//
	// Reload 需要它去决定读哪些分支的树。早先把赋值放在 Reload 之后，
	// 结果首次加载拿到的是空分支，只能靠各处写死的字面量兜底——
	// 那些兜底分支永远不会被走到，却让人以为默认分支是代码里的常量。
	branch := cfg.DefaultBranch
	if branch == "" && cfgFile != nil {
		branch = cfgFile.Server.DefaultBranch
	}
	if branch == "" {
		branch = gitstore.DefaultBranch
	}

	s := &Server{
		cfg:           cfg,
		dataDir:       cfg.DataDir,
		weights:       w,
		projects:      NewProjectStore(cfg.DataDir),
		tokens:        tokens,
		rev:           rev,
		cfgFile:       cfgFile,
		defaultBranch: branch,
		passwords:     pwStore,
		grants:        auth.NewGrantStore(cfg.DataDir),
		uiLimiter:     newUILoginLimiter(),
		lock:          startupLock,

		listens:   FromConfig(listenBlocks(cfgFile)),
		bcast:     newBroadcaster(),
		treeWatch: newTreeWatchHub(),
		done:      make(chan struct{}),
		// 默认视为「没有维护循环」：New 之后没 Start 就 Close 的路径
		// （测试里很常见）不该在这里永久阻塞。
		maintainDone: func() chan struct{} {
			ch := make(chan struct{})
			close(ch)
			return ch
		}(),
	}
	if lookup != nil {
		s.cfgLookup.Store(lookup)
	}
	// 会话表：改口令时清空全部会话，所以它必须能看到口令状态。
	s.sessions = auth.NewSessionStore(12*time.Hour, s.passwords.MustChange)

	if cfgFile != nil {
		rems := cfgFile.Remotes
		s.remotes.Store(&rems)
	}
	if err := s.Reload(); err != nil {
		return nil, fmt.Errorf("build initial index: %w", err)
	}

	// 语义向量侧与 BM25 平行异步启动。
	// 缺失模型配置时静默跳过，不会阻碍服务正常启动。
	s.initSemantic()

	if s.UIRoutesEnabled() {
		if plain, created, berr := s.passwords.Bootstrap(os.Getenv("ZERO_MEMCP_UI_PASS")); berr != nil {
			return nil, fmt.Errorf("initialize WebUI password: %w", berr)
		} else if created {
			s.cfg.UIBootstrapPassword = plain
		}
		if !s.passwords.Exists() {
			return nil, errors.New("WebUI is enabled but its password could not be initialized")
		}
	}

	startupOK = true
	return s, nil
}

// Reload 读出所有裸仓库的当前分支并重建倒排索引。
//
// 存储层完全是 Git 裸仓库：
//   - 项目 = <data>/repos/<name>.git，配一个就有，想加几个加几个
//   - 树来自每个项目的当前分支 refs/heads/<defaultBranch>
//   - 规则与内容的分野写在 tree entry 的 mode 里（100755 = 规则，100644 = 内容）
//
// 这里没有「身份」这个概念：分支名只是这份记忆的版本线。
func (s *Server) Reload() error {
	// 优雅热重载：先试着重新解析配置文件。
	//
	// 黄金规则（用户指定的）：reload 语法失败或校验不通过时，
	// 绝对不能覆盖内存里正在生效的旧配置！
	// 不崩溃、不降级，把错误记入警告日志，保留旧表继续工作。
	// 只有当新文件完整解析成功、校验无误之后，才原子替换指针。
	if s.cfg.ConfigFile != "" {
		if newFile, err := config.Load(s.cfg.ConfigFile); err != nil {
			s.logf("warning: reload failed to re-parse config (keeping the existing config): %v", err)
		} else {
			s.cfgMu.Lock()
			s.cfgFile = newFile
			s.cfgMu.Unlock()
			s.cfgLookup.Store(newFile.Build())
			rems := newFile.Remotes
			s.remotes.Store(&rems)
		}
	}

	defaultBranch := s.defaultBranch

	trees, err := s.loadGitTrees(defaultBranch)
	if err != nil {
		return err
	}

	// 判重表要先于索引使用：重启后靠它记住「这条已经写过了」。
	s.seedDedup(trees)

	idx := index.New()
	for _, t := range trees {
		for _, b := range t.Indexable() {
			data, err := s.readBlock(b)
			if err != nil {
				return fmt.Errorf("read %s: %w", b.Addr(), err)
			}
			// 时间优先取正文里的 at:/created:，回退 commit 时间。
			//
			// 批量导入会把几千篇历史切片打成同一个 commit 时间，
			// 那份时间对排序和时效权重都是噪声；正文里那个时刻
			// 才是这条记忆代表的事件发生时间。
			stamp := b.Time
			if at := sliceAtTime(string(data), b.Path); at > 0 {
				stamp = at
			}
			idx.AddWithTime(b.ID(), string(data), stamp)

			// SKILL.md 永远解析策略，不看 b.Rule。
			//
			// 踩过的坑：早先要求 b.Rule 为真才解析，而 daily 的 SKILL.md
			// 声明了 inject: never（它不注入上下文），于是被判为非规则、
			// 整个跳过——里面的 stream: true / target / kinds 声明全落空。
			//
			// 「是不是规则文件（要不要注入）」和「它声不声明写入策略」
			// 是两件事：流水项目恰恰是最不该注入的，但它必须声明 stream。
			if b.Path == "SKILL.md" || strings.HasSuffix(b.Path, "/SKILL.md") {
				s.policies.Store(t.Project, manifest.Parse(t.Project, data))
			}
		}
	}

	// 比较上一观察水位与文件系统的当前 heads。分支变化的真相在 refs；
	// 内存水位只是优化在线通知，丢了以后插件可以凭 last commit 调 since。
	events := s.treeEventsSinceBaseline()

	s.mu.Lock()
	s.trees = trees
	s.idx = idx
	s.mu.Unlock()

	// 先更新水位再广播：并发的下一次 Reload 不会重复发同一批事件。
	s.rememberTreeHeads()
	for _, ev := range events {
		s.broadcastTreeEvent(ev)
	}
	return nil
}

// DescribeProjects 返回一句话的项目概览，供启动日志用。
//
// 用配置声明的集合，不用磁盘扫描：启动瞬间仓库可能一个都还没建，
// 但配置里已经声明了项目——打印「无项目」会让人以为配置没生效。
func (s *Server) DescribeProjects() string {
	projs := s.declaredProjects()
	if len(projs) == 0 {
		return "(no projects)"
	}
	return fmt.Sprintf("projects %s (%s)", strings.Join(projs, " "), s.defaultBranch)
}

// Start 按配置里的每个 [[listen]] 块分别起一个监听器。
//
// 每个端口有自己的策略（允许哪些动词、是否强制 token、端口级挂载表）。
// 权限边界落在端口上而不是代码里的 if：
// 一个端口没配写动词，它的路由里就没有写代码可走。
func (s *Server) Start() error {
	// Start 只在真正启动维护循环时重建这个通道。
	// 用 mutex 保护：Start 可能在 goroutine 里跑，Close 同时在等它。
	s.startMu.Lock()
	if !s.maintainStarted {
		s.maintainStarted = true
		s.maintainDone = make(chan struct{})
		go s.maintain(s.maintainDone)
	}
	s.startMu.Unlock()

	blocks := s.listens
	if len(blocks) == 0 {
		// 单端口模式恒开管理面。
		//
		// 这是唯一一个不需要「用户去改配置」的入口：容器里、npm-wrapper 后面
		// 都拿不到命令行，配置写错一行就永久锁死。所以默认必须有管理面，
		// 想隔离它的人可以显式写 [[listen]] 把 webui 放到专用回环端口。
		blocks = []ListenBlock{{Addr: s.cfg.Listen, Allow: []string{"*"}, WebUI: true}}
	}

	var servers []*http.Server
	for _, blk := range blocks {
		ln, err := net.Listen("tcp", blk.Addr)
		if err != nil {
			// 先关掉已经起来的，避免半个服务在跑
			for _, srv := range servers {
				_ = srv.Close()
			}
			return fmt.Errorf("cannot listen on %s (port is probably in use; exiting immediately): %w", blk.Addr, err)
		}
		if len(servers) == 0 {
			s.setListener(ln)
		}
		srv := &http.Server{Handler: s.muxFor(blk)}
		servers = append(servers, srv)
		go func(srv *http.Server, ln net.Listener, addr string) {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				s.logf("server on port %s stopped: %v", addr, err)
			}
		}(srv, ln, blk.Addr)
	}

	s.netMu.Lock()
	s.httpServers = servers
	s.netMu.Unlock()
	// 阻塞直到 Close
	<-s.done
	return nil
}

// muxFor 为一个监听端口构建路由。
func (s *Server) muxFor(blk ListenBlock) http.Handler {
	mux := http.NewServeMux()
	// 管理面只在显式声明 webui 的端口上注册。
	//
	// 没开的端口上这些路径根本不存在（404），而不是「存在但拒绝」。
	// 结构性不存在比判断更可靠：它连「这里有个管理台」都不告诉人。
	if blk.WebUI {
		s.uiMux(mux)
		mux.HandleFunc("/", s.handleIndex)
		mux.HandleFunc("/api", s.handleUIAPICompat)
	}
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/identity", s.handleIdentity)
	mux.HandleFunc("/projects", s.handleProjects)
	mux.HandleFunc("/recall", s.handleRecall)
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/repos/", s.handleGitHTTP)
	return &portPolicy{blk: blk, next: mux}
}

// handleUIAPICompat 让旧面板的 /api?action=... 走管理面鉴权。
//
// 为什么必须换掉：早先 /api 用的是通用 authenticate()，
// 而那条路径在本机且未强制鉴权时会降级成 root——
// 于是同机任何进程不带凭证就能通过 /api?action=read 读走全部正文。
// 面板属于管理面，判据只能是会话。
func (s *Server) handleUIAPICompat(w http.ResponseWriter, r *http.Request) {
	if !s.uiAuthGuard(w, r, false) {
		return
	}
	s.handleAPIAsUI(w, r)
}

// portPolicy 是端口级策略中间件。
type portPolicy struct {
	blk  ListenBlock
	next http.Handler
}

func (p *portPolicy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// HTTP Git 端点：读与写分别要 allow 里显式声明。
	// 推送入口默认关闭——多一个能写东西的入口就是多一处要守的边界。
	if strings.HasPrefix(r.URL.Path, "/repos/") {
		readOK := p.allows("git-upload-pack")
		pushOK := p.allows("git-receive-pack")
		if !readOK && !pushOK {
			http.Error(w, "HTTP Git is not enabled on this port", http.StatusForbidden)
			return
		}
		if strings.Contains(r.URL.Path, "git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack" {
			if !pushOK {
				http.Error(w, "git-receive-pack is not enabled on this port (push is disabled by default)", http.StatusForbidden)
				return
			}
		} else if !readOK {
			http.Error(w, "fetch is not enabled on this port", http.StatusForbidden)
			return
		}
		p.next.ServeHTTP(w, r.WithContext(withPortPolicy(r.Context(), p.blk)))
		return
	}
	// 其余路径（含 MCP）：把端口策略挂进 context，供动词层读取
	p.next.ServeHTTP(w, r.WithContext(withPortPolicy(r.Context(), p.blk)))
}

// allows 报告本端口是否允许某个能力。
func (p *portPolicy) allows(name string) bool {
	return allowedOnPort(p.blk, name)
}

// Close 停止全部监听器。
func (s *Server) Close() error {
	s.doneOnce.Do(func() { close(s.done) })
	// 停语义索引的后台追平：它会读仓库，必须在关仓库之前停。
	s.stopSemanticMaintainer()
	s.netMu.Lock()
	srvs := s.httpServers
	s.netMu.Unlock()
	var lastErr error
	for _, srv := range srvs {
		if err := srv.Close(); err != nil {
			lastErr = err
		}
	}
	// 先等维护循环退出，再关仓库。
	//
	// 反过来的顺序（先关仓库）会让正在跑的 repo.Commit 撞上
	// 已关闭的写者：提交本身写的是 ref 和对象，不经过写队列，
	// 于是它会在一个逻辑上已关闭的仓库上继续写。
	s.startMu.Lock()
	done := s.maintainDone
	s.startMu.Unlock()
	if done != nil {
		<-done
	}
	if s.projects != nil {
		if err := s.projects.Close(); err != nil && lastErr == nil {
			lastErr = err
		}
	}
	// Flush read-frequency state before releasing the instance lock.
	//
	// weights writes lazily (500ms after the last read). Closing without
	// draining that timer drops the most recent reads, so a restart quietly
	// loses ranking signal that was already paid for by real reads.
	if s.weights != nil {
		if err := s.weights.Close(); err != nil && lastErr == nil {
			lastErr = err
		}
	}
	// 释放实例锁要放在最后：仓库写者还没关完时放开锁，
	// 下一个进程就能在旧进程还在收尾时进来。
	if s.lock != nil {
		if err := s.lock.Release(); err != nil && lastErr == nil {
			lastErr = err
		}
	}
	return lastErr
}

// Addr 返回实际监听的地址。
//
// 用随机端口（:0）启动时，真实端口只有监听之后才知道，
// 所以要读 listener 而不是配置。
func (s *Server) Addr() string {
	if ln := s.getListener(); ln != nil {
		return ln.Addr().String()
	}
	return s.cfg.Listen
}

// handleIdentity 返回每轮要注入的身份块全文。
//
// 这是唯一一个"内容会进入系统提示"的接口，所以它只吐 inject: every_turn
// 且 x 位为真的块——没有 x 的内容是资料，不该变成规则。
func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 注入哪些块完全由这张凭证决定：能力位 x 决定给不给规则，
	// 挂载表决定看得见哪些项目与分支。没有任何 query 参数能改变它。
	//
	// 这里曾经有过 ?profile=，它算出一个值再也没被用过——承诺在那儿，
	// 实现不在。身份是签发时的事，不是请求里的一件事。
	type block struct {
		Addr string `json:"addr"`
		Text string `json:"text"`
	}
	var blocks []block

	// 注入是凭证的 x 能力：没有 x 的凭证拿不到任何规则块。
	//
	// 这让「这张 token 受不受宪法约束」变成签发时就能决定的事，
	// 而不依赖客户端自觉。没有 x 的 agent 在自己的世界里没有规则——
	// 这是 root 编排出来的状态，不是它自己选的。
	if !caller.canInject() {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"blocks": []block{}, "cap_x": false})
		return
	}

	// 注入必须以「一个项目 = 一条身份线」为单位。
	//
	// 踩过的坑：早先这里直接遍历 s.trees 全量叠加，而 s.trees 是
	// (项目, 分支) 粒度的——同一个项目有几个分支就有几棵树。
	// blackegg/me 有 zero / main / chat 三条分支，于是三套身份
	// 同时进了同一个系统提示：「我是简」与「我是 QQ 群助手」并列，
	// 同一份规则还被重复注入两遍（addr 完全相同）。模型的反应是
	// 不知道自己是谁。
	//
	// 分支在这里是「版本线」，不是「并存在的多个身份」。
	// 一次对话只能归属一个身份。
	byProject := make(map[string][]*memory.Tree)
	var projects []string
	for _, t := range s.trees {
		// 挂载表之外的仓库连「存在」都不该被看到
		if !caller.Mount.Visible(t.Project, t.Branch) {
			continue
		}
		if _, ok := byProject[t.Project]; !ok {
			projects = append(projects, t.Project)
		}
		byProject[t.Project] = append(byProject[t.Project], t)
	}
	sort.Strings(projects)

	for _, proj := range projects {
		t := s.identityTreeFor(caller, proj, byProject[proj])
		if t == nil {
			continue
		}
		for _, b := range t.Injectable() {
			if !s.executableBlock(caller, b) {
				continue
			}
			if caller.Mount.ToVirtual(b.Project, b.Branch, b.Path) == "" {
				continue
			}
			data, err := s.readBlock(b)
			if err != nil {
				continue
			}
			// 地址翻成虚拟路径：chroot 之内看到的是自己的世界
			blocks = append(blocks, block{Addr: caller.Mount.ToVirtual(b.Project, b.Branch, b.Path), Text: string(data)})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"blocks": blocks})
}

// identityTreeFor 从一个项目的多条分支里选出唯一要注入身份的那一条。
//
// 优先级：
//  1. 凭证在挂载表里钉住的分支——这是签发时写死的意图；
//  2. 服务端默认分支（通常是 zero）；
//  3. 按分支名字典序取第一个（只为确定性，不表达偏好）。
//
// 不选“把所有分支合起来”：那样得到的是几个互相矛盾的身份，不是更全的身份。
func (s *Server) identityTreeFor(c Caller, project string, trees []*memory.Tree) *memory.Tree {
	if len(trees) == 0 {
		return nil
	}
	if len(trees) == 1 {
		return trees[0]
	}

	// 1. 凭证钉住的分支。通配 "*" 不算钉住：它表示“读得见全部分支”，
	//    而身份只能归属一条。
	if bs := c.Mount.BranchesOf(project); len(bs) == 1 {
		if want := bs[0]; want != "*" && want != "" {
			for _, t := range trees {
				if t.Branch == want {
					return t
				}
			}
		}
	}

	// 2.5 终局模型：main 分支（人格所在）优先于默认分支。
	//
	// 新模型下「项目 = 人格，main = 该人格的本体设定（me/）」。
	// 老仓库的默认分支叫 zero（zero-agi 时代的历史遗留），里面装的是
	// 旧分层 SKILL.md——那不是人格，是检索配置。人格在 main 上。
	// 所以 main 必须排在 defaultBranch 之前：一个同时有 zero 和 main
	// 的仓库，人格应该从 main 来；没有 main 的老仓库才退回默认分支。
	for _, t := range trees {
		if t.Branch == "main" {
			return t
		}
	}

	// 3. 服务端默认分支。
	for _, t := range trees {
		if t.Branch == s.defaultBranch {
			return t
		}
	}

	// 3. 确定性兜底。
	sorted := append([]*memory.Tree(nil), trees...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Branch < sorted[j].Branch })
	return sorted[0]
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]any{
		"ok":      true,
		"service": "zero-memcp",
		"version": Version,
		"listen":  s.Addr(),
	}

	// 健康检查是未鉴权入口（探活要用它），所以它只说「我活着」。
	// 索引规模是跨项目的信息，只有携带凭证时才给。
	caller, authErr := s.authenticate(r)
	if authErr == nil {
		s.mu.RLock()
		totalBlocks, injectBlocks, docs := 0, 0, 0
		for _, t := range s.trees {
			if !s.allowed(caller, t.Project, t.Branch, configBitRead) {
				continue
			}
			totalBlocks += len(t.Blocks)
			injectBlocks += len(t.Injectable())
			docs += len(t.Indexable())
		}
		s.mu.RUnlock()
		resp["docs_indexed"] = docs
		// 词表规模是全局的：它不按项目切分，所以只给全命名空间的调用者。
		if caller.Mount.Empty() {
			resp["terms_indexed"] = s.idx.Terms()
		}
		resp["total_blocks"] = totalBlocks
		resp["inject_blocks"] = injectBlocks
	}

	// clock 是通知计数器，不是文档数。
	// 重启归零没关系——客户端看到读数比基线小，自己重新对齐。
	resp["clock"] = s.Clock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	defer s.mu.RUnlock()

	type projItem struct {
		Name   string `json:"name"`
		Blocks int    `json:"blocks"`
		Rules  int    `json:"rules"`
	}
	var list []projItem
	for _, t := range s.trees {
		if !s.allowed(caller, t.Project, t.Branch, configBitRead) {
			continue
		}
		st := t.Stats()
		name := caller.Mount.ToVirtual(t.Project, t.Branch, "")
		if name == "" {
			name = t.Project
		}
		list = append(list, projItem{
			Name:   name,
			Blocks: st.Blocks,
			Rules:  st.Rules,
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"projects": list,
		"clock":    s.Clock(),
	})
}

// handleRecall 给插件热路径使用：POST 纯 JSON，毫秒级返回，不走 MCP 协议开销。
func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Text  string  `json:"text"`
		Limit int     `json:"limit"`
		Floor float64 `json:"floor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 3
	}

	s.mu.RLock()
	// 多取一些，权限与挂载过滤后再截断——否则受限凭证的 limit=3
	// 会因为前 3 条都被过滤掉而返回空，看起来像"搜不到"。
	hits := s.idx.SearchCovered(req.Text, req.Limit*4, req.Floor, recallMinCoverage, s.weightCount, nil)
	s.mu.RUnlock()

	type HitView struct {
		Addr    string   `json:"addr"`
		Score   float64  `json:"score"`
		Matched []string `json:"matched"`
	}
	view := make([]HitView, 0, req.Limit)
	for _, h := range hits {
		if len(view) >= req.Limit {
			break
		}
		// 用命中那条自己的分支判权限，不能用 s.defaultBranch。
		//
		// 索引键是 project@branch/path，分支就在地址里。早先一律拿
		// s.defaultBranch（服务端默认分支）去判，后果是所有「不是默认分支」
		// 的命中在权限层被静默丢弃：受限身份把内容写进自己的分支，
		// read 读得到、log 看得到，recall 却永远无命中——不报错，
		// 只是搜不到，像一个不会说话的记忆库。
		proj, br, _, _ := memory.SplitID(h.Addr)
		if proj == "" {
			proj = s.projectOfAddr(h.Addr)
			br = s.defaultBranch
		}
		if !s.allowed(caller, proj, br, configBitRead) {
			continue
		}
		virt := s.addrToVirtual(h.Addr, br, caller)
		if virt == "" {
			continue
		}
		matched := make([]string, len(h.Matched))
		for j, m := range h.Matched {
			matched[j] = string(m)
		}
		view = append(view, HitView{Addr: virt, Score: h.Score, Matched: matched})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"hits": view})
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// Reject admin sessions before authenticating.
	//
	// This has to run first, not only on the failure path: with a valid Bearer
	// present, authenticate() succeeds and a request carrying both credentials
	// would be served. The two channels are meant to be mutually exclusive, and
	// "Bearer wins" would make it impossible to tell whether a call came from
	// the agent credential or the human admin session.
	if uiForbiddenIfBearer(w, r) {
		return
	}
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32700, "message": "parse error"},
		})
		return
	}

	result, err := s.dispatchMCP(r.Context(), req.Method, req.Params, caller)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error":   map[string]any{"code": -32603, "message": err.Error()},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result":  result,
	})
}

func (s *Server) dispatchMCP(ctx context.Context, method string, params json.RawMessage, c Caller) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":      "zero-memcp",
				"version":   "0.1.0",
				"local":     true,
				"transport": "streamable-http",
			},
		}, nil

	case "notifications/initialized":
		return map[string]any{}, nil

	case "tools/list":
		// 只暴露 term。
		//
		// 本服务实现 termcp 协议：单工具 term({cmd})，动词在命令空间里分发。
		//
		// termcp 协议的约定是「一个入口，一行命令」：recall / read / put
		// 都是 term 的动词，不是独立的工具。逐个暴露等于给每个动词开一个
		// 口子，工具表变长、每轮都出现在提示词里，而能力一点没多。
		//
		// 声明面等于能力面：tools/list 里看得见 term，就只能调 term。
		//
		// 插件自己不走这条路——它有专门的 /identity 和 /recall 路由。
		return map[string]any{
			"tools": []map[string]any{
				{
					"name": "term",
					"description": "termcp protocol entry point (implemented by this service). Accepts one line of DSL:\n" +
						"  recall <text> [--scope T] semantic search (returns addresses and excerpts)\n" +
						"  read <addr> [<addr>...]  read full text; accepts multiple addresses\n" +
						"  grep <regex>             search full text with surrounding lines (better than recall for symbols, numbers, and names)\n" +
						"  log [N]                  show dates and summaries of the most recent N entries (default 5)\n" +
						"  ls [date]                list entries for a date\n" +
						"  put [<tree>] -- $TEXT    write content to a memory tree (pass content through vars/args, not in the DSL)\n" +
						"  put <tree> --ref <addr>  append a reference to existing memory in a tree\n" +
						"  trees <project>[@<branch>]  view the tree directory\n" +
						"  remind [<tree>]          watch a tree and receive notifications when it changes\n" +
						"  since <tree> <commit>    retrieve tree changes since a commit\n" +
						"  status                   index size\n" +
						"  help [<verb>]            show usage for a verb; an empty command shows the overview\n" +
						"This is your persistent memory: a successful put is stored locally and remains available after a restart.\n" +
						"When to use each: ask \"what did I think about X?\" -> recall;\n" +
						"search for exact strings (IP, port, QQ number, function name) -> grep;\n" +
						"ask \"what was I working on recently?\" -> log.\n" +
						"If the first word is the name of a downstream MCP, pass the rest through unchanged.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd": map[string]any{
								"type":        "string",
								"description": "DSL command line, e.g. 'recall napcat' or 'put -- $TEXT'. Pass content in vars/args and reference it with $NAME / $1; a reference must occupy a whole word.",
							},
							"args": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Positional arguments referenced as $1, $2, etc. in cmd. A reference must occupy a whole word. Values are not escaped or concatenated; pass content here instead of embedding it in cmd.",
							},
							"vars": map[string]any{
								"type":                 "object",
								"additionalProperties": map[string]any{"type": "string"},
								"description":          "Named arguments referenced as $NAME in cmd. The same rule as args applies: a reference must occupy a whole word, and every supplied value must be referenced.",
							},
							"content": map[string]any{
								"type":        "boolean",
								"default":     true,
								"description": "Whether to return a text projection for agents. Set to false for programmatic consumers.",
							},
							"structured_content": map[string]any{
								"type":        "boolean",
								"default":     false,
								"description": "Whether to return a structured projection for programs. Combine with content:false to return only JSON.",
							},
						},
						"required": []string{"cmd"},
					},
				},
			},
		}, nil

	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, fmt.Errorf("parse call arguments: %w", err)
		}
		return s.callTool(ctx, call.Name, call.Arguments, c)

	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage, c Caller) (any, error) {
	// 只接受 term。
	//
	// tools/list 只声明了 term，这里也必须只接受 term——声明面和能力面
	// 不一致是这类服务最容易出的事：清单上看不到的东西还能调用，
	// 而调用方不会知道自己用了一个没被声明的接口。
	if name != "term" {
		return nil, fmt.Errorf("unknown tool: %s (this service exposes only term)", name)
	}
	var a struct {
		Cmd string `json:"cmd"`

		// Args / Vars 是参数表：DSL 只写引用，正文从这两张表进来。
		//
		// 为什么不在服务端做「智能转义」：一旦允许正文和语法拼在同一行，
		// 切分器迟早会把数据当成命令。分开之后，值里有什么字符都不影响语法。
		Args []string          `json:"args"`
		Vars map[string]string `json:"vars"`

		// For 是调用者的会话 UUID（分身身份）。
		//
		// 它刻意放在参数里而不是 cmd 里：身份是传输层的事实，
		// 不是 DSL 的一部分。模型看不见它，也就不可能被诱导去冒充别人。
		For string `json:"for"`

		// Groups 是调用者能接的活（"qq"、"home"）。
		// 同样是传输层事实：用来收 @group 的信，不参与 DSL。
		Groups []string `json:"groups"`
		// Content 控制是否返回文本投影。默认 true。
		//
		// 关掉它是给程序消费者用的：它读 structuredContent，不读排版好的
		// 自然语言，那份文本对它是纯浪费的 token 和带宽。
		Content *bool `json:"content"`
		// StructuredContent 控制是否返回结构化投影。默认 false。
		//
		// 默认关，理由跟 termcp 一样：两份投影承载同样的信息，
		// 都发等于每次回复收两份钱，而人类看到的是同一答案的两种方言。
		StructuredContent bool `json:"structured_content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("parse term arguments: %w", err)
	}

	// 参数替换发生在任何动词分发之前，且只做一次。
	//
	// 替换后的文本不再被当作模板重新展开：否则值里一个 $NAME 会变成
	// 第二次替换的入口，等于把数据提升成指令。
	cmd := a.Cmd
	if len(a.Args) > 0 || len(a.Vars) > 0 {
		expanded, err := expandArgs(a.Cmd, a.Vars, a.Args)
		if err != nil {
			return nil, err
		}
		cmd = expanded
	} else if strings.Contains(a.Cmd, "$") {
		// 没给参数表却写了引用：拒绝，而不是把它当字面量。
		// 静默保留 $1 会让调用方以为参数生效了。
		if _, err := expandArgs(a.Cmd, nil, nil); err != nil {
			return nil, err
		}
	}

	wantText := true
	if a.Content != nil {
		wantText = *a.Content
	}

	// 两个投影都不要 = 空的碗。
	//
	// 不替调用方猜意图：content:false 的合理含义是"我要 JSON"，
	// 但如果它连 structuredContent 也不要，这条命令就一个字节都不需要返回。
	// 那不是宽容，那是参数写错了——直接报错让它知道。
	if !wantText && !a.StructuredContent {
		return nil, errors.New("content:false and structured_content:false leave no projection to return; enable at least one")
	}

	// 端口级动词白名单：这个端口没开这个动词，就在这里拒绝。
	//
	// 检查放在 execTerm 之前而不是里面：execTerm 拿不到 request context，
	// 而端口策略是跟着请求走的。放在这里也保证「拒绝」发生在任何
	// 副作用之前——动词自己都还没开始跑。
	// Check EVERY statement's verb, not just the first.
	//
	// A line of several statements is several capabilities. Reading only the
	// leading verb would let "recall x; put y" through a read-only port: the
	// write would run while the policy only ever looked at "recall".
	for _, stmt := range splitStatements(cmd) {
		verb := verbOf(stmt)
		if verb == "" {
			continue
		}
		if blk, ok := portPolicyFrom(ctx); ok && !allowedOnPort(blk, verb) {
			return nil, portError(blk, verb)
		}
	}

	viewer := Viewer{Channel: "mcp"}
	out, err := s.execTerm(cmd, viewer, c)
	if err != nil {
		return nil, err
	}
	return buildToolResult(out, wantText, a.StructuredContent), nil
}

// buildToolResult 按调用方要的投影组装工具结果。
//
// MCP 规范允许 content 为空数组，所以「只要 JSON」是一个合法的结果，
// 而不是缺字段。
func buildToolResult(r TermResult, wantText, wantStructured bool) map[string]any {
	res := map[string]any{}

	if wantText {
		res["content"] = []map[string]any{
			{"type": "text", "text": r.Text},
		}
	} else {
		res["content"] = []map[string]any{}
	}

	if wantStructured {
		if r.Structured != nil {
			res["structuredContent"] = r.Structured
		} else {
			// 没有专用结构化投影的动词（help 之类）退回一个退化形态：
			// 把文本塞进 text 字段。它不漂亮，但保持了「要 structuredContent
			// 就一定有 structuredContent」这个承诺，不会让调用方拿到空。
			res["structuredContent"] = map[string]any{"text": r.Text}
		}
	}

	return res
}

func (s *Server) writeNote(kind, text, session, source string, c Caller, explicit string) (addr, project, branch string, err error) {
	return s.writeNoteGit(kind, text, session, source, c, explicit, "")
}

// TermResult 是一条命令的两种投影。
//
// Text 给智能体读：自然语言、带摘要、排版友好。
// Structured 给程序读：字段固定、可直接解析、没有客套话。
//
// 为什么一个命令要产出两份而不是一份：
//
//	它们服务两个不同的消费者。模型读 JSON 会浪费 token 还要自己拼语义；
//	程序读自然语言要写正则，而且一旦措辞改了它就崩。
//	分开发，两边都不用为对方那份付钱——由调用方用 content /
//	structured_content 两个开关各取所需。
type TermResult struct {
	Text       string
	Structured any
	// Empty reports "ran fine, found nothing" - the state a caller branches on.
	//
	// A query with no hits is not an error, and treating it as one would be
	// wrong twice over: the reply is still a valid answer, and a credential that
	// may legitimately see nothing must not look like a broken request. So
	// conditionals need a flag rather than an error, and they cannot infer it
	// from the text either - "no matches" is a rendering, and rewording it would
	// silently invert every "fall back to the next layer" rule built on it.
	Empty bool
}

// execTerm runs a DSL line, which may hold several statements.
//
// One statement is the common case and behaves exactly as before. Several -
// separated by ';' or newline - are executed in order in a single request, so a
// caller that wants "the same query in three layers" spends one round trip
// instead of three. That matters more here than elsewhere: recall returns three
// hits, and a layered memory means asking fact, record and thought separately.
//
// "&&" and "||" make the next question answerable, which is "should I ask the
// cheaper layer at all?". A layered memory is mostly queried at the top:
//
//	recall q --scope proj@fact || recall q --scope proj@record || recall q --scope proj@thought
//
// runs one query most of the time and descends only when the layer above came
// back empty. Compare the unconditional ';' form, which always pays three
// searches and always returns three blocks of mostly-redundant text into the
// prompt.
//
// WHAT COUNTS AS SUCCESS: a statement that ran without error AND found
// something. This is deliberately not shell's rule. In shell, `no matches`
// would be exit 1; here a query that legitimately finds nothing is a valid
// answer, not a failure, and if the fallback keyed on errors it would never
// fire - every recall succeeds, so `||` would always be skipped. TermResult.Empty
// carries the distinction.
//
// Chains are left-associative like shell: "a && b || c" runs b only if a
// succeeded, and c only if the result of "a && b" was a miss. A skipped
// statement does not run and contributes no text.
//
// Execution stops at the first error, whatever the operator was. A later
// statement may depend on an earlier one (write then commit), so continuing past
// a failure would produce a result that looks half-applied and reports success.
// "||" is for "found nothing", not for "this credential cannot ask here" - those
// stay fatal, because silently falling through a permission error would report
// memory from a layer the caller was never allowed to reach as if it were a
// normal answer.
//
// The empty command still returns the overview: with nothing to split, the
// single-statement path is what answers it.
func (s *Server) execTerm(line string, v Viewer, c Caller) (TermResult, error) {
	stmts := splitStatementsWithOps(line)
	if len(stmts) <= 1 {
		return s.execStatement(line, v, c)
	}

	var text strings.Builder
	results := make([]any, 0, len(stmts))
	wrote := false

	// truthy is the accumulated result of the chain so far, in the sense above.
	truthy := true
	for i, st := range stmts {
		run := true
		switch st.Op {
		case "&&":
			run = truthy
		case "||":
			run = !truthy
		}
		if !run {
			// Skipped: it keeps the chain's truth value, exactly as shell would
			// keep the exit status of the branch that did not run.
			results = append(results, map[string]any{
				"statement": st.Cmd,
				"verb":      verbOf(st.Cmd),
				"op":        st.Op,
				"skipped":   true,
			})
			continue
		}

		res, err := s.execStatement(st.Cmd, v, c)
		if err != nil {
			return TermResult{}, fmt.Errorf("statement %d of %d (%q): %w", i+1, len(stmts), verbOf(st.Cmd), err)
		}
		truthy = !res.Empty

		if wrote {
			text.WriteString("\n\n")
		}
		wrote = true
		// Name the statement above its result.
		//
		// Without it the blocks run together and the one fact the caller needs -
		// which layer a hit came from - is lost: addresses are virtualised, so
		// "za/2026-11-03/...md" does not say fact from thought. It costs a short
		// line per statement and buys back the whole point of asking per layer.
		text.WriteString(describeStatement(st.Cmd))
		text.WriteString("\n")
		text.WriteString(res.Text)
		results = append(results, map[string]any{
			"statement": st.Cmd,
			"verb":      verbOf(st.Cmd),
			"op":        st.Op,
			"empty":     res.Empty,
			"text":      res.Text,
			"result":    res.Structured,
		})
	}
	return TermResult{
		Text: text.String(),
		Structured: map[string]any{
			"statements": results,
			"count":      len(results),
		},
	}, nil
}

// execStatement runs exactly one statement.
func (s *Server) execStatement(line string, v Viewer, c Caller) (TermResult, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		// 空命令返回总览。跟 termcp 一样：不知道该问什么的时候，
		// 空调用就是「我现在能做什么」这个问题的答案。
		return TermResult{Text: helpOverview()}, nil
	}
	parts := strings.Fields(line)
	verb := parts[0]
	rest := strings.TrimSpace(strings.TrimPrefix(line, verb))

	// defaultBranch 是写入与 branch/tag 类动词的目标分支线。
	//
	// 唯一来源是服务端的 s.defaultBranch（启动参数或配置里的默认值）。
	// 没有逐请求覆盖身份的路：
	// 它让「这条记忆属于谁」看起来是请求属性，而真相是分支。
	defaultBranch := s.defaultBranch

	switch verb {
	case "help":
		// help [<verb>] [--all]
		//
		// 为什么需要它：工具描述是静态声明，写在 tools/list 里，
		// 每轮都占提示词预算；而「这个动词怎么用」是一个按需问题，
		// 只在使用者真的要用的时候才值得花一次往返。
		//
		// 声明面放能做什么，help 放怎么做。两者分开，预算才不浪费。
		arg := strings.TrimSpace(rest)
		if arg == "" || arg == "--all" {
			if arg == "--all" {
				return TermResult{Text: helpAll()}, nil
			}
			return TermResult{Text: helpOverview()}, nil
		}
		if detail, ok := helpVerb(strings.Fields(arg)[0]); ok {
			return TermResult{Text: detail}, nil
		}
		return TermResult{}, fmt.Errorf("unknown verb %q\n\n%s", strings.Fields(arg)[0], helpOverview())

	case "recall":
		// recall <text>
		rest = restorePayload(rest)
		//
		// 返回地址 + 摘要片段，不是光秃秃的地址。
		//
		// 踩过的坑：只回 "addr (score)" 时，模型看不出这篇讲什么，必须逐个 read，
		// 读三四篇之后它就换 rg 去翻目录了——不是它不想用记忆工具，是记忆工具
		// 每回答一个问题都要它做 N 次额外往返。给摘要能把 N 次 read 压成 0 次。
		s.mu.RLock()
		// 先按「这张凭证看得见什么」收窄候选集合，再打分排序。
		//
		// 顺序不能反：索引是所有分支的合集，先取全局 top-N 再按权限
		// 抛弃，等于让其他凭证的内容占掉名额——最终结果可能是
		// 「明明有 360 篇相关记录，recall 返回无命中」。
		// 在这张凭证的可见范围内算出 top-N，才是它该看到的最佳答案。
		allowedHit := func(addr string) bool {
			proj, br, _, ok := memory.SplitID(addr)
			if !ok {
				proj = s.projectOfAddr(addr)
				br = s.defaultBranch
			}
			if !c.Mount.Visible(proj, br) {
				return false
			}
			return s.allowed(c, proj, br, configBitRead)
		}
		query, scope := splitScope(rest)
		scopeMatch := func(addr string) bool {
			if scope == "" {
				return true
			}
			return addrUnderScope(addr, scope)
		}
		hits := s.idx.SearchCovered(query, 3, 0, recallMinCoverage, s.weightCount, func(addr string) bool {
			return allowedHit(addr) && scopeMatch(addr)
		})
		permHits := make([]index.Hit, 0, len(hits))
		for _, h := range hits {
			// 命中项的地址形如 project@branch/path——
			// 必须用它自己的分支判权限，不能用 s.defaultBranch。
			//
			// 踩过的坑：早先一律按 s.defaultBranch（服务端默认分支）判定，
			// 于是受限身份写进自己分支的内容虽然进了索引，
			// 却因为「它的挂载表里没有那条分支」被全部过滤掉——
			// 症状是 recall 永远「无命中」，而内容明明写成功了。
			proj, br, _, ok := memory.SplitID(h.Addr)
			if !ok {
				proj = s.projectOfAddr(h.Addr)
				br = s.defaultBranch
			}
			if s.allowed(c, proj, br, configBitRead) {
				permHits = append(permHits, h)
			}
		}
		hits = permHits
		// 摘要必须在锁内取完：snippet 读的是 s.trees，而 Reload 会在
		// s.mu.Lock() 里把它整体换掉。在锁外调用等于读一份随时会
		// 被替换的切片——-race 能稳定复现（TestRecallConcurrentWithReload）。
		snips := make([]string, len(hits))
		for i, h := range hits {
			snips[i] = s.snippet(h.Addr, query, 240)
		}
		s.mu.RUnlock()
		if len(hits) == 0 {
			return TermResult{
				Text:       "no matches",
				Structured: map[string]any{"hits": []any{}, "count": 0},
				Empty:      true,
			}, nil
		}
		var sb strings.Builder
		structHits := make([]any, 0, len(hits))
		for i, h := range hits {
			_, hbr, _, okBr := memory.SplitID(h.Addr)
			if !okBr {
				hbr = s.defaultBranch
			}
			virt := s.addrToVirtual(h.Addr, hbr, c)
			if virt == "" {
				continue
			}
			sb.WriteString(fmt.Sprintf("%s (%.2f)\n", virt, h.Score))
			snip := snips[i]
			if snip != "" {
				for _, line := range strings.Split(snip, "\n") {
					sb.WriteString("    " + line + "\n")
				}
			}
			sb.WriteString("\n")
			structHits = append(structHits, map[string]any{
				"addr":    virt,
				"score":   h.Score,
				"snippet": snip,
			})
		}
		return TermResult{
			Text: strings.TrimRight(sb.String(), "\n"),
			Structured: map[string]any{
				"hits":  structHits,
				"count": len(structHits),
			},
		}, nil

	case "srecall":
		// srecall <text> [--scope <tree>]
		//
		// 语义增强召回：向量（跨语言、摸「为什么」）+ BM25（精确字面）
		// 两路融合。命中 thought 时自动带出同轮 record。
		return s.execSRecall(rest, c)

	case "read":
		// read <addr>
		//
		// recall: off 的目录读不出来。之前只解析不执行——声明写着"不许被翻到"，
		// read 照样给全文，等于那句声明是装饰。
		// read <addr> [<addr> ...]
		//
		// 支持一次读多篇：模型经常需要同时看几篇才能回答一个问题，
		// 一篇一次往返会让它转向 rg。
		fields := strings.Fields(rest)
		for i := range fields {
			fields[i] = restorePayload(fields[i])
		}
		if len(fields) == 0 {
			return TermResult{}, errors.New("read syntax: read <addr> [<addr> ...]")
		}
		s.mu.RLock()
		defer s.mu.RUnlock()

		var out strings.Builder
		missing := []string{}
		for _, addr := range fields {
			proj, br, path, err := s.resolveAddr(addr, c)
			if err == nil && !s.allowed(c, proj, br, configBitRead) {
				out.WriteString("read: " + addr + " -> " + s.denyReason(c, proj, br, configBitRead) + "\n\n")
				continue
			}
			if err != nil {
				missing = append(missing, addr+"("+err.Error()+")")
				continue
			}
			repo, err := s.projects.Get(proj)
			if err != nil {
				missing = append(missing, addr+"(project does not exist)")
				continue
			}
			d, err := repo.Read(br, path)
			if err != nil {
				// 没写 @branch 时跨分支兜一次。
				//
				// 为什么需要：地址写成 zero-agi/2026-10-28/x.md 时，默认分支通常
				// 是零号分支（没内容），而文件其实在 record 或 thought 上。
				// 用户不知道「默认分支」是哪个，也不该知道——他手里只有路径。
				// 唯一命中就直接给；多分支都有就列出来消歧；都没有才报 not found。
				if strings.Contains(addr, "@") {
					missing = append(missing, addr+"(not found)")
					continue
				}
				cands := s.branchCandidates(repo, path, c, proj)
				switch len(cands) {
				case 0:
					missing = append(missing, addr+"(not found)")
					continue
				case 1:
					found, ferr := repo.Read(cands[0], path)
					if ferr != nil {
						missing = append(missing, addr+"(not found)")
						continue
					}
					br, d = cands[0], found
				default:
					// 多分支同路径：不猜，把选择权交回去。
					missing = append(missing, fmt.Sprintf("%s(found on %s; add @branch)", addr, strings.Join(cands, ", ")))
					continue
				}
			}
			// 权重键必须与索引键一致（带分支），否则 read 强化对
			// 不带分支的地址记账、索引却按带分支的键查，永远对不上。
			//
			// 只 bump 一个键。旧版同时 bump MakeID 键和 proj/path 旧键，
			// 同一份记忆的计数被劈成两半，权重按一半算。
			nowUnix := time.Now().Unix()
			s.weights.Bump(memory.MakeID(proj, br, path), nowUnix)
			if len(fields) > 1 {
				out.WriteString("==== " + addr + " ====\n")
			}
			out.WriteString(string(d))
			out.WriteString("\n")
		}
		if out.Len() == 0 {
			// 路径越界不叫 not found。
			//
			// 「不可见」与「不存在」必须说清楚：前者是凭证边界，
			// 后者是记忆库真的没有。混在一起会让人以为记忆丢了，
			// 于是去翻仓库——而事实是这张凭证压根不该看见它。
			return TermResult{}, fmt.Errorf("%s", strings.Join(missing, ", "))
		}
		if len(missing) > 0 {
			out.WriteString("\n[not retrieved] " + strings.Join(missing, ", ") + "\n")
		}
		return TermResult{Text: out.String()}, nil

	case "put":
		return s.execPut(rest, c)

	case "remind":
		return s.execRemind(rest, c)

	case "trees":
		return s.execTrees(rest, c)

	case "since":
		return s.execSince(rest, c)

	case "remote-key":
		return s.execRemoteKey(rest, c)

	case "remote-secret":
		return s.execRemoteSecret(rest, c)

	case "remote":
		return s.execRemote(rest, c)

	case "fetch":
		return s.execRemoteFetch(rest, c)

	case "merge":
		if !c.canWrite() {
			return TermResult{}, fmt.Errorf("merge requires w capability")
		}
		return s.execMerge(rest, c)

	case "push":
		if !c.canWrite() {
			return TermResult{}, fmt.Errorf("push requires w capability")
		}
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		return s.execPush(rest, c)

	case "sync":
		return s.execSync(rest, c)

	case "branch":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// branch [<project> [<new> [from <src>]]]
		out, err := s.execBranch(rest, c, defaultBranch)
		return TermResult{Text: out}, err

	case "tag":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// tag [-d] <project> [<name> [<ref>]]
		out, err := s.execTag(rest, c, defaultBranch)
		return TermResult{Text: out}, err

	case "cp":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// cp <src>@<br>/<path> ... -> <dst>@<br>/[<path>]
		out, err := s.execCrossBranch(rest, c, false)
		return TermResult{Text: out}, err

	case "mv":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// mv <src>@<br>/<path> ... -> <dst>@<br>/[<path>]
		out, err := s.execCrossBranch(rest, c, true)
		return TermResult{Text: out}, err

	case "chmod":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// chmod <project>[@<branch>]/<path> rule|content
		out, err := s.execChmod(rest, c, defaultBranch)
		return TermResult{Text: out}, err

	case "delete":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// delete <project>@<branch>/<path> [...]
		out, err := s.execDelete(rest, c, defaultBranch)
		return TermResult{Text: out}, err

	case "lock":
		// lock <project>@<branch>
		out, err := s.execLock(rest, c)
		return TermResult{Text: out}, err

	case "unlock":
		// unlock <project>@<branch>
		out, err := s.execUnlock(rest, c)
		return TermResult{Text: out}, err

	case "archive":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// archive <project>@<branch> [-> <project>@<as>]
		out, err := s.execArchive(rest, c)
		return TermResult{Text: out}, err

	case "reset":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// reset <project>@<branch> from <project>@<src>
		out, err := s.execReset(rest, c)
		return TermResult{Text: out}, err

	case "batch":
		// batch {"ops":[{...}]}
		out, err := s.execBatch(rest, c, defaultBranch)
		return TermResult{Text: out}, err

	case "diff":
		// diff <project>@<branch>  或  diff a@br -> b@br
		out, err := s.execDiff(rest, c, defaultBranch)
		return TermResult{Text: out}, err

	case "commit":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// commit [<project>[@<branch>]] [-m <msg>]
		out, err := s.execCommit(rest, c, defaultBranch)
		return TermResult{Text: out}, err

	case "write":
		if err := s.requireBranchWide(rest, c); err != nil {
			return TermResult{}, err
		}
		// write <project>[@<branch>]/<path> -- <内容>
		out, err := s.execWrite(rest, c, defaultBranch)
		return TermResult{Text: out}, err

	case "config":
		// config show | config path | config put <TOML>
		out, err := s.execConfig(rest, c)
		return TermResult{Text: out}, err

	case "reload":
		if err := s.requireRootGlobal(c, 'w'); err != nil {
			return TermResult{}, err
		}
		if err := s.Reload(); err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: "index rebuilt"}, nil

	case "whoami":
		return TermResult{Text: s.execWhoami(c)}, nil

	case "users":
		if err := s.requireRootGlobal(c, 'r'); err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: s.execUsers().Text}, nil

	case "status":
		// 全局索引量对不同凭证来说也是信息：受限身份只该看到
		// 自己可见范围内的切片数，否则它能数出别人的记忆规模。
		//
		// 词表规模是**全局**事实（它不按项目切分），所以与 /health
		// 同一口径：只给全命名空间的调用者。早先这里无条件返回，
		// 于是 health 刻意隐藏的东西在 status 里一读就到。
		s.mu.RLock()
		docs := 0
		for _, t := range s.trees {
			if !s.allowed(c, t.Project, t.Branch, configBitRead) {
				continue
			}
			docs += len(t.Indexable())
		}
		// Terms 必须在锁内读：Reload 会在 s.mu.Lock() 里整体替换 s.idx，
		// 锁外调用就是和写方赛跑（-race 可复现）。
		terms := 0
		if c.Mount.Empty() {
			terms = s.idx.Terms()
		}
		s.mu.RUnlock()
		text := fmt.Sprintf("docs: %d", docs)
		if c.Mount.Empty() {
			text = fmt.Sprintf("docs: %d, terms: %d", docs, terms)
		}
		return TermResult{
			Text: text,
			Structured: map[string]any{
				"docs":  docs,
				"terms": terms,
				"clock": s.Clock(),
			},
		}, nil

	case "grep":
		// grep <pattern> [--scope <tree>]
		//
		// 正则匹配全文，带上下文。
		// 模型换 bash 的头号原因就是想搜精确串（比如 "254203716" 或 "BlackEgg"）
		// 而 BM25 不擅长这类纯符号/专有名词。给它原生的 grep，它就不用换工具。
		//
		// --scope 收窄搜索范围：记忆树大了之后，不分范围的全量扫
		// 会把命中堆到与本次无关的项目上。
		//
		// 未知选项必须报错：`grep -c . zero-agi` 早先会把 "-c" 编进正则，
		// 最后返回 "no matches"——调用方据此得出「库里没有」，而事实只是参数错了。
		if bad, ok := unknownFlag(rest); ok {
			return TermResult{}, fmt.Errorf("grep: unknown flag %q (supported: --scope)", bad)
		}
		pattern, scope := splitScope(rest)
		if pattern == "" {
			return TermResult{}, errors.New("grep syntax: grep <pattern> [--scope <tree>]")
		}
		grepText, grepFound, grepErr := s.execGrep(pattern, scope, c)
		if grepErr != nil {
			return TermResult{}, grepErr
		}
		return TermResult{Text: grepText, Empty: !grepFound}, nil

	case "log":
		// log [N]
		//
		// 类似 git log：按时间倒序看最近 N 篇（缺省 5 篇）的日期、主题、摘要。
		// 模型想了解"最近做了什么"时，不用去 ls 目录。
		n := 5
		if rest != "" {
			if parsed, err := strconv.Atoi(strings.TrimPrefix(rest, "-")); err == nil && parsed > 0 {
				n = parsed
			}
		}
		logText, logErr := s.execLog(n, c)
		if logErr != nil {
			return TermResult{}, logErr
		}
		return TermResult{Text: logText}, nil

	case "ls":
		// ls [date]
		//
		// 列出某天（缺省今天/最新一天）的全部切片。
		lsText, lsErr := s.execLs(rest, c)
		if lsErr != nil {
			return TermResult{}, lsErr
		}
		return TermResult{Text: lsText}, nil

	default:
		if suggestion := suggestVerb(verb); suggestion != "" {
			return TermResult{}, fmt.Errorf("unknown verb %q; did you mean %q?", verb, suggestion)
		}
		return TermResult{}, fmt.Errorf("unknown verb %q (if connected to a downstream MCP, this should be passed through)\n\n%s", verb, helpOverview())
	}
}

func textResult(s string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": s},
		},
	}
}

// Report 打印扫描结果，用于验证目录结构与声明是否符合预期。
//
// 这一屏输出比读文档快：它直接告诉你哪些块会被注入、哪些进索引、哪些被跳过。
func (s *Server) Report(w io.Writer, c Caller) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.trees {
		if !c.Mount.Visible(t.Project, t.Branch) {
			continue
		}
		fmt.Fprintf(w, "\n=== %s ===\n", t.Project)
		for _, b := range t.Blocks {
			tag := "index"
			if b.Injectable() {
				tag = "inject"
			}
			if v := c.Mount.ToVirtual(b.Project, b.Branch, b.Path); v != "" {
				fmt.Fprintf(w, "  [%s] %7d  %s\n", tag, b.Size, v)
			}
		}
		st := t.Stats()
		fmt.Fprintf(w, "  --- %d blocks / %d rules / %d index bytes\n",
			st.Blocks, st.Rules, st.IndexBytes)
	}
	fmt.Fprintf(w, "\nIndex: %d blocks / %d terms\n", s.idx.Docs(), s.idx.Terms())
}

// snippet 取一块内容的摘要：优先返回命中词附近的那段，取不到就返回开头。
//
// 只读文件，不改任何状态——摘要预览不该被算作"读过"（那样 recall 会把权重也改了）。
func (s *Server) snippet(addr, query string, limit int) string {
	var block *memory.Block
	// 索引地址是 project@branch/path；Tree.Find 接受的是仓库内路径。
	// 以前把整个 ID 直接交给 Find，分支前缀永远匹配不上，
	// recall 有命中却永远不给摘要。先拆 ID，按项目+分支找树，再传 path。
	if project, branch, path, ok := memory.SplitID(addr); ok {
		for _, t := range s.trees {
			if t.Project == project && t.Branch == branch {
				if b, found := t.Find(path); found {
					bb := b
					block = &bb
				}
				break
			}
		}
	} else {
		for _, t := range s.trees {
			if b, ok := t.Find(addr); ok {
				bb := b
				block = &bb
				break
			}
		}
	}
	if block == nil {
		return ""
	}
	data, err := s.readBlock(*block)
	if err != nil {
		return ""
	}
	text := string(data)

	// 剥掉 frontmatter：模型要看的是内容，不是元数据。
	if strings.HasPrefix(text, "---") {
		if i := strings.Index(text[3:], "\n---"); i >= 0 {
			text = text[3+i+4:]
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	// 命中词附近优先：找查询里任意一个词第一次出现的位置，往前退一点。
	// 中文没有空格，逐词找位置比做窗口切分可靠。
	best := -1
	for _, w := range strings.Fields(query) {
		if idx := strings.Index(text, w); idx >= 0 && (best < 0 || idx < best) {
			best = idx
		}
	}
	// 也试整串
	if best < 0 {
		if idx := strings.Index(text, strings.TrimSpace(query)); idx >= 0 {
			best = idx
		}
	}

	start := 0
	if best > 120 {
		start = best - 60
	}
	r := []rune(text[start:])
	if len(r) > limit {
		r = r[:limit]
		return string(r) + "..."
	}
	return string(r)
}

func (s *Server) weightCount(addr string) int {
	if s.weights == nil {
		return 0
	}
	// 单一规范键：只查 MakeID 形式。旧版还会兜底查 project/path，
	// 那是为兼容「双写两个键」而存在的；双写已取消，兜底也一并去掉。
	return s.weights.Count(addr)
}

// execGrep 在所有可 recall 的块里正则搜索，返回命中行及其上下文。
//
// scope 非空时只搜该范围下的条目。范围可以写成 project、
// project@branch 或 project@branch/subtree，与 recall 的 --scope 同一套语义。
//
// 为什么要先剥参数再编译正则：早先整个 rest 直接当正则用，
// 于是 `grep 1729 --scope daily` 会把 "--scope daily" 也编进模式里，
// 结果是必然零命中——而报错文本是「无命中」，看上去像内容不存在。
func (s *Server) execGrep(pattern, scope string, c Caller) (string, bool, error) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return "", false, fmt.Errorf("invalid regular expression: %w", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var sb strings.Builder
	totalHits := 0
	for _, tr := range s.trees {
		// 只判挂载表会漏掉两层：凭证 cap 与 TOML 权限。
		// grep 读的是正文，判据必须与 read 完全一致。
		if !s.allowed(c, tr.Project, tr.Branch, configBitRead) {
			continue
		}
		for _, b := range tr.Blocks {
			if b.Hidden {
				continue
			}
			// 范围收窄：把块拼回索引地址形状，复用与 recall 同一套判定，
			// 避免两处各写一份前缀匹配而慢慢走偏。
			if scope != "" && !addrUnderScope(b.ID(), scope) {
				continue
			}
			data, err := s.readBlock(b)
			if err != nil {
				continue
			}
			lines := strings.Split(string(data), "\n")
			blockHit := false
			for i, line := range lines {
				if re.MatchString(line) {
					if !blockHit {
						blockHit = true
						sb.WriteString("-- " + c.Mount.ToVirtual(b.Project, b.Branch, b.Path) + " --\n")
					}
					// 打印上下文：命中行前一行 + 命中行 + 后一行
					start := i - 1
					if start < 0 {
						start = 0
					}
					end := i + 2
					if end > len(lines) {
						end = len(lines)
					}
					for j := start; j < end; j++ {
						prefix := "   "
						if j == i {
							prefix = " > "
						}
						sb.WriteString(fmt.Sprintf("%4d%s%s\n", j+1, prefix, lines[j]))
					}
					totalHits++
					if totalHits >= 30 {
						sb.WriteString("\n[truncated: too many matches; showing the first 30]\n")
						return sb.String(), true, nil
					}
				}
			}
			if blockHit {
				sb.WriteString("\n")
			}
		}
	}
	if totalHits == 0 {
		// The bool is what a conditional branches on; the text is only a
		// rendering. Returning just the string would force callers to compare
		// against "no matches", and rewording that message would silently invert
		// every "fall back to the next layer" rule.
		return "no matches", false, nil
	}
	return strings.TrimRight(sb.String(), "\n"), true, nil
}

// execLog 按时间倒序看最近 N 篇的摘要。
func (s *Server) execLog(n int, c Caller) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var all []memory.Block
	for _, tr := range s.trees {
		if !s.allowed(c, tr.Project, tr.Branch, configBitRead) {
			continue
		}
		for _, b := range tr.Blocks {
			all = append(all, b)
		}
	}
	// 排序依据：正文里的事件时间优先，commit 时间兜底。
	//
	// 为什么不能只看 commit 时间：批量导入会把几千篇历史切片打成
	// 同一个时间戳，排序随即退化成路径字典序——用户以为看到的是
	// 「最近」，其实是文件名最大的那批。正文里的 at:/created: 才是
	// 这条记忆真正代表的那一刻。
	//
	// 时间从索引的 docTime 表里取（建索引时已按正文算好），不在
	// 这里逐篇读盘：log 是高频动词，对 3700 篇逐个解包 frontmatter
	// 会把一次列举变成一次全库扫描。
	type entry struct {
		b memory.Block
		t int64
	}
	entries := make([]entry, len(all))
	for i, b := range all {
		t := s.idx.DocTime(b.ID())
		if t == 0 {
			t = b.Time
		}
		entries[i] = entry{b: b, t: t}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].t != entries[j].t {
			return entries[i].t > entries[j].t
		}
		return entries[i].b.Path > entries[j].b.Path
	})
	if len(entries) > n {
		entries = entries[:n]
	}

	var sb strings.Builder
	for i, e := range entries {
		b := e.b
		t := time.Unix(e.t, 0).Format("2006-01-02 15:04:05")
		virt := c.Mount.ToVirtual(b.Project, b.Branch, b.Path)
		if virt == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%d] %s (%s, %d bytes)\n", i+1, virt, t, b.Size))
		if snip := s.snippet(b.Addr(), "", 160); snip != "" {
			for _, line := range strings.Split(snip, "\n") {
				sb.WriteString("    " + line + "\n")
			}
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// execLs 列出某天的切片。
func (s *Server) execLs(rest string, c Caller) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// ls [date] [--scope <tree>]
	//
	// 踩过的坑：早先把 rest 整个当日期用。调用方写
	// `ls 2026-11-05 --scope BlackEggAI/zero@main.record:review`，
	// day 就成了整根字符串，Contains 永远不命中，返回
	// "no matching entries"——而文件明明在那里。这与 grep/recall
	// 的 --scope 处理对齐：先拆 scope，剩下的是日期。
	if bad, ok := unknownFlag(rest); ok {
		return "", fmt.Errorf("ls: unknown flag %q (supported: --scope)", bad)
	}
	day, scope := splitScope(rest)
	if i := strings.Index(day, " "); i >= 0 {
		// 剩余参数多于一个（如两个日期）——不猜，直接报。
		return "", fmt.Errorf("ls: expected one date, got %q", day)
	}
	var scopeMatch func(addr string) bool
	if scope == "" {
		scopeMatch = func(string) bool { return true }
	} else {
		scopeMatch = func(addr string) bool { return addrUnderScope(addr, scope) }
	}

	var matches []memory.Block
	for _, tr := range s.trees {
		if !s.allowed(c, tr.Project, tr.Branch, configBitRead) {
			continue
		}
		for _, b := range tr.Blocks {
			// 用虚拟地址匹配，不用 b.Path：后者是树内路径，
			// 两个仓库里同名的日期目录会被混在一起。
			virt := c.Mount.ToVirtual(b.Project, b.Branch, b.Path)
			if virt == "" {
				continue
			}
			if day != "" && !strings.Contains(virt, day) {
				continue
			}
			if !scopeMatch(memory.MakeID(b.Project, b.Branch, b.Path)) {
				continue
			}
			matches = append(matches, b)
		}
	}
	if len(matches) == 0 {
		return "no matching entries", nil
	}
	sort.Slice(matches, func(i, j int) bool {
		return c.Mount.ToVirtual(matches[i].Project, matches[i].Branch, matches[i].Path) <
			c.Mount.ToVirtual(matches[j].Project, matches[j].Branch, matches[j].Path)
	})

	// 输出上限。
	//
	// 为什么必须有：`ls 2026-11-03` 在真实库上会匹配几千篇切片，
	// 全量倾倒 26 万字符直接打爆上下文窗口——而且它的每一行只值 60 字节，
	// 却按完整通道计费。给个稳定上限，超出部分只报数字，让人换更窄的查询。
	total := len(matches)
	show := matches
	if total > lsMaxEntries {
		show = matches[:lsMaxEntries]
	}

	var sb strings.Builder
	for _, b := range show {
		virt := c.Mount.ToVirtual(b.Project, b.Branch, b.Path)
		if virt == "" {
			virt = b.Path
		}
		sb.WriteString(fmt.Sprintf("%-50s %6d bytes\n", virt, b.Size))
	}
	if total > lsMaxEntries {
		fmt.Fprintf(&sb, "... and %d more entries (total %d). Narrow the query.", total-lsMaxEntries, total)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// ── 逻辑时钟（进程内 atomic）──────────────────────────────────
//
// 唯一职责：告诉客户端「你上次看过之后，有人写过东西」。
// 单一计数器即可——时钟是「记忆库更新过」，不需要按身份分裂。
func (s *Server) Clock() int64 {
	return s.clock.Load()
}

func (s *Server) bumpClock() int64 {
	n := s.clock.Add(1)
	s.bcast.Send("clock", fmt.Sprintf(`{"clock":%d}`, n))
	return n
}

// ExecLine 执行一行记忆 DSL，返回文本结果。
//
// 这是给内嵌调用者使用的快捷入口：它用 root 权限、默认分支、
// 无端口策略约束直接跑一条动词。宿主如果自己已经做了鉴权，就不需要
// 再走一遍 HTTP 中间件；如果它需要收窄，可以通过带上下文的变种调。
func (s *Server) ExecLine(ctx context.Context, line string) (string, error) {
	c := s.rootCaller()
	viewer := Viewer{Channel: "embedded"}
	res, err := s.execTerm(line, viewer, c)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// Mux 返回完整的路由表，供嵌入式宿主整体挂载。
func (s *Server) Mux() http.Handler {
	// 管理面是否出现，只能由调用方显式要求。
	//
	// 早先这里写死 WebUI: true，于是 SDK 宿主把整个 mux 挂出去时
	// 会带上一套管理接口——而口令引导只在配置里声明 webui 时才发生，
	// 结果是「管理台在、但没人能登录」，同时它还多占了 / 与 /api
	// 两个路由，把宿主的同名路径挤掉。
	return s.muxFor(ListenBlock{Allow: []string{"*"}, WebUI: true})
}

// ReposMux 只暴露 HTTP Git 端点（/repos/）。
func (s *Server) ReposMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", s.handleGitHTTP)
	return mux
}

// requireBranchWide 阻止路径受限挂载做分支级操作。
//
// 挂载可以收窄到分支内的一个子树（Path，见 Mount.LookupPath）。
// 子树分身读写自己世界里的内容没问题——那条路走 resolveAddr 的
// PathVisible 检查；但 branch/tag/reset/push 这些动词的作用域是
// 整条分支，一个只挂了 record/贴吧 的凭证去 reset main.record
// 等于用自己的门钥匙拆整栋楼。
//
// 全命名空间（空挂载表）与整分支挂载直接放行。
func (s *Server) requireBranchWide(rest string, c Caller) error {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return nil
	}
	addr := fields[0]
	proj, br, _, err := s.parseBranchAddr(addr)
	if err != nil {
		proj = addr
		br = ""
	}
	if !c.Mount.BranchWide(proj, br) {
		return fmt.Errorf("%s@%s is mounted with a path subtree (%q); this operation acts on the whole branch and is not available to this credential",
			proj, br, c.Mount.rootOf(proj, br))
	}
	return nil
}
