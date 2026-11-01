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
	"github.com/open-mcp-ai/zero-memcp/internal/index"
	"github.com/open-mcp-ai/zero-memcp/internal/manifest"
	"github.com/open-mcp-ai/zero-memcp/internal/memory"
	"github.com/open-mcp-ai/zero-memcp/internal/weights"
)

// DefaultPort 是约定的固定端口：哈代-拉马努金数 1729。
const DefaultPort = 1729

// Config 启动配置。
type Config struct {
	// DataDir 是数据根。<data>/system/ 放密钥与吊销名单，
	// <data>/repos/<name>.git 放裸仓库。
	DataDir string

	// ConfigFile 是配置文件路径。
	ConfigFile string

	// Listen 监听地址，缺省 "127.0.0.1:1729"。
	Listen string

	// Endpoint 本端点标识，例如 local / blackbox。
	Endpoint string

	// WeightsPath 阅读频次状态文件。
	WeightsPath string

	// DefaultProfile 是没指定 profile 时的默认人格名。
	DefaultProfile string

	// EnforceAuth 即使在 127.0.0.1 也强制要求带 JWT Token。
	EnforceAuth bool
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

	// persona 是当前写入的分支名。所有仓库共用它。
	//
	// 为什么是单一 persona 而不是每用户一份：现在「多用户」只表示
	// 「哪些人能用哪些仓库」，不是「每个人有自己的记忆分支」。
	// 一台机器上跑的记忆就是一份，分支名是它的版本线。
	persona string

	// weights 记录哪些块真的被读过。只统计 read，不统计 recall——
	// recall 只是列出候选，瞄一眼没读说明它没帮上忙。
	weights *weights.State

	// projects 管理所有裸仓库（Git 存储模式）。
	projects *ProjectStore

	// tokens 负责 JWT 签发与验签。
	tokens *auth.TokenStore

	// rev 是吊销名单。
	rev *auth.Revocations

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
	// 值带分支前缀，避免 chat 分支的同一条把 dev 分支的判掉。
	dedup sync.Map // "project@branch:h8" -> path

	// lastCommits 记录上次看到的各分支 HEAD（键见 treeHeadKey），用来发现外部提交。
	lastCommits sync.Map // treeHeadKey(project, branch) -> commit hash

	// done 通知后台维护循环退出。
	done     chan struct{}
	doneOnce sync.Once

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
		return nil, fmt.Errorf("非法地址 %q: %w", cfg.Listen, err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil, fmt.Errorf("安全拒绝：只能绑定回环地址，得到 %q", host)
	}

	if cfg.DataDir == "" {
		return nil, errors.New("必须给数据目录：-data 或 -config 里的 [server] data")
	}

	w, err := weights.Open(cfg.WeightsPath)
	if err != nil {
		return nil, fmt.Errorf("打开权重状态: %w", err)
	}

	// 签名密钥必须落盘。每次启动重新生成会让所有已签发的 token
	// 同时失效，而那种故障的表现是「所有设备一起 401」——
	// 排查的人会先怀疑网络和配置，最后才想到密钥。
	var tokens *auth.TokenStore
	var rev *auth.Revocations
	if cfg.DataDir != "" {
		systemDir := filepath.Join(cfg.DataDir, "system")
		secret, err := auth.LoadOrCreateSecret(filepath.Join(systemDir, "jwt.key"))
		if err != nil {
			return nil, fmt.Errorf("加载密钥: %w", err)
		}
		tokens = auth.NewTokenStore(secret)
		rev = auth.NewRevocations(filepath.Join(systemDir, "revoked"))
	} else {
		tokens = auth.NewTokenStore(nil)
	}

	s := &Server{
		cfg:      cfg,
		dataDir:  cfg.DataDir,
		weights:  w,
		projects: NewProjectStore(cfg.DataDir),
		tokens:   tokens,
		rev:      rev,
		cfgFile:  cfgFile,

		listens:   FromConfig(listenBlocks(cfgFile)),
		bcast:     newBroadcaster(),
		treeWatch: newTreeWatchHub(),
		done:      make(chan struct{}),
	}
	if lookup != nil {
		s.cfgLookup.Store(lookup)
	}
	if cfgFile != nil {
		rems := cfgFile.Remotes
		s.remotes.Store(&rems)
	}
	if err := s.Reload(); err != nil {
		return nil, fmt.Errorf("构建初始索引: %w", err)
	}

	// persona 在启动时定一次。
	//
	// 只认配置里的 default_persona，别的一概不管：
	// 分支名是「这份记忆的版本线」，不是「谁在用」。
	s.mu.Lock()
	if cfg.DefaultProfile != "" {
		s.persona = cfg.DefaultProfile
	} else {
		s.persona = "zero"
	}
	s.mu.Unlock()

	return s, nil
}

// Reload 读出所有裸仓库的当前分支并重建倒排索引。
//
// 存储层完全是 Git 裸仓库：
//   - 项目 = <data>/repos/<name>.git，配一个就有，想加几个加几个
//   - 树来自每个项目的当前分支 refs/heads/<persona>
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
			s.logf("警告：reload 重新解析配置失败（保留原配置不变）：%v", err)
		} else {
			s.cfgMu.Lock()
			s.cfgFile = newFile
			s.cfgMu.Unlock()
			s.cfgLookup.Store(newFile.Build())
			rems := newFile.Remotes
			s.remotes.Store(&rems)
		}
	}

	persona := s.persona
	if persona == "" {
		persona = "zero"
	}

	trees, err := s.loadGitTrees(persona)
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
				return fmt.Errorf("读 %s: %w", b.Addr(), err)
			}
			idx.AddWithTime(b.ID(), string(data), b.Time)

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

// DescribeProfiles 返回一句话的项目概览，供启动日志用。
//
// 用配置声明的集合，不用磁盘扫描：启动瞬间仓库可能一个都还没建，
// 但配置里已经声明了项目——打印「无项目」会让人以为配置没生效。
func (s *Server) DescribeProfiles() string {
	projs := s.declaredProjects()
	if len(projs) == 0 {
		return "（无项目）"
	}
	return fmt.Sprintf("项目 %s（%s）", strings.Join(projs, " "), s.persona)
}

// Start 按配置里的每个 [[listen]] 块分别起一个监听器。
//
// 每个端口有自己的策略（允许哪些动词、是否强制 token、端口级挂载表）。
// 权限边界落在端口上而不是代码里的 if：
// 一个端口没配写动词，它的路由里就没有写代码可走。
func (s *Server) Start() error {
	go s.maintain()

	blocks := s.listens
	if len(blocks) == 0 {
		blocks = []ListenBlock{{Addr: s.cfg.Listen, Allow: []string{"*"}}}
	}

	var servers []*http.Server
	for _, blk := range blocks {
		ln, err := net.Listen("tcp", blk.Addr)
		if err != nil {
			// 先关掉已经起来的，避免半个服务在跑
			for _, srv := range servers {
				_ = srv.Close()
			}
			return fmt.Errorf("无法监听 %s（端口很可能被占用，按规范立即退出）: %w", blk.Addr, err)
		}
		if len(servers) == 0 {
			s.setListener(ln)
		}
		srv := &http.Server{Handler: s.muxFor(blk)}
		servers = append(servers, srv)
		go func(srv *http.Server, ln net.Listener, addr string) {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				s.logf("端口 %s 服务结束: %v", addr, err)
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
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api", s.handleAPI)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/identity", s.handleIdentity)
	mux.HandleFunc("/profiles", s.handleProfiles)
	mux.HandleFunc("/recall", s.handleRecall)
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/repos/", s.handleGitHTTP)
	return &portPolicy{blk: blk, next: mux}
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
			http.Error(w, "这个端口没有开启 HTTP Git", http.StatusForbidden)
			return
		}
		if strings.Contains(r.URL.Path, "git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack" {
			if !pushOK {
				http.Error(w, "这个端口没有开启 git-receive-pack（推送入口默认关闭）", http.StatusForbidden)
				return
			}
		} else if !readOK {
			http.Error(w, "这个端口没有开启拉取", http.StatusForbidden)
			return
		}
		p.next.ServeHTTP(w, r)
		return
	}
	// 其余路径（含 MCP）：把端口策略挂进 context，供动词层读取
	p.next.ServeHTTP(w, r.WithContext(withPortPolicy(r.Context(), p.blk)))
}

// allows 报告本端口是否允许某个能力。
func (p *portPolicy) allows(name string) bool {
	for _, a := range p.blk.Allow {
		if a == "*" || a == name {
			return true
		}
	}
	return false
}

// Close 停止全部监听器。
func (s *Server) Close() error {
	s.doneOnce.Do(func() { close(s.done) })
	s.netMu.Lock()
	srvs := s.httpServers
	s.netMu.Unlock()
	var lastErr error
	for _, srv := range srvs {
		if err := srv.Close(); err != nil {
			lastErr = err
		}
	}
	if s.projects != nil {
		if err := s.projects.Close(); err != nil && lastErr == nil {
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

	// 这里曾经有一个 ?profile= 参数，用来「选一份身份档案」。
	//
	// 它是假的：算出来的 persona 只赋给一个再也没被读过的变量，
	// 过滤条件的真相是挂载表（X-Zero-Role 决定），不是这个 query。
	// 于是 serve -profile 与 /identity?profile=xxx 都能接受，
	// 都什么也不做——承诺在那儿，实现不在。
	//
	// 现在删掉它。要换身份视角，换角色，不换参数：
	// 身份由凭证的挂载表表达，不由请求里的一个字符串表达。
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

	for _, t := range s.trees {
		// 挂载表之外的仓库连「存在」都不该被看到
		if !caller.Mount.Visible(t.Project, t.Branch) {
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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]any{
		"ok":      true,
		"service": "zero-memcp",
		"version": "0.1.0",
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

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
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
	hits := s.idx.SearchWeighted(req.Text, req.Limit*4, req.Floor, s.weightCount)
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
		// 用命中那条自己的分支判权限，不能用 s.persona。
		//
		// 索引键是 project@branch/path，分支就在地址里。早先一律拿
		// s.persona（服务端人格分支）去判，后果是所有「不是人格分支」
		// 的命中在权限层被静默丢弃：chat 角色 put 写进 daily@chat，
		// read 读得到、log 看得到，recall 却永远无命中——不报错，
		// 只是搜不到，像一个不会说话的记忆库。
		proj, br, _, _ := memory.SplitID(h.Addr)
		if proj == "" {
			proj = s.projectOfAddr(h.Addr)
			br = s.persona
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
					"description": "termcp 协议入口（本服务实现 termcp 协议）。收一行 DSL：\n" +
						"  recall <text> [--scope T] 语义联想（返回地址 + 摘要片段）\n" +
						"  read <addr> [<addr>...]  读全文，可一次多篇\n" +
						"  grep <regex>             正则扫全文，带前后一行上下文（找符号/数字/专名用它，比 recall 准）\n" +
						"  log [N]                  按时间倒序看最近 N 篇（缺省 5）的日期和摘要\n" +
						"  ls [date]                列出某天的切片\n" +
						"  put [<tree>] -- $TEXT    把正文写进记忆树（正文走 vars/args，不与 DSL 混写）\n" +
						"  put <tree> --ref <addr>  在树里追加已有记忆的引用\n" +
						"  trees <project>[@<branch>]  查看树目录\n" +
						"  remind [<tree>]          锚定一棵树；它变动时收到通知\n" +
						"  since <tree> <commit>    取回自上次提交以来的树差分\n" +
						"  status                   索引规模\n" +
						"  help [<verb>]            这个动词怎么用；空命令也返回总览\n" +
						"这是你的持久记忆体：put 成功即已落进本地持久记录，重启后仍可读。\n" +
						"什么时候用哪个：问「关于X我之前怎么想的」→ recall；\n" +
						"找精确串（IP、端口、QQ号、函数名）→ grep；\n" +
						"问「最近做到哪了」→ log。\n" +
						"第一个词若是下游 MCP 的名字，剩余部分原样透传。",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd": map[string]any{
								"type":        "string",
								"description": "DSL 命令行，例如 'recall napcat' 或 'put -- $TEXT'。正文放进 vars/args，用 $NAME / $1 引用；引用必须独占一个词。",
							},
							"args": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "位置参数，供 cmd 里的 $1、$2 引用。引用必须独占一个词，值不做任何转义或拼接——正文放这里，不要拼进 cmd。",
							},
							"vars": map[string]any{
								"type":                 "object",
								"additionalProperties": map[string]any{"type": "string"},
								"description":          "命名参数，供 cmd 里的 $NAME 引用。与 args 同一条规则：引用独占一个词，传了就必须被引用。",
							},
							"content": map[string]any{
								"type":        "boolean",
								"default":     true,
								"description": "是否返回文本投影（给智能体读）。程序消费者可设为 false。",
							},
							"structured_content": map[string]any{
								"type":        "boolean",
								"default":     false,
								"description": "是否返回结构化投影（给程序读）。与 content:false 搭配可只拿 JSON。",
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
			return nil, fmt.Errorf("解析 call 参数: %w", err)
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
		return nil, fmt.Errorf("unknown tool: %s（本服务只暴露 term）", name)
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
		return nil, fmt.Errorf("解析 term 参数: %w", err)
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
		return nil, errors.New("content:false 与 structured_content:false 同时成立时没有任何投影可返回；至少要一个")
	}

	// 端口级动词白名单：这个端口没开这个动词，就在这里拒绝。
	//
	// 检查放在 execTerm 之前而不是里面：execTerm 拿不到 request context，
	// 而端口策略是跟着请求走的。放在这里也保证「拒绝」发生在任何
	// 副作用之前——动词自己都还没开始跑。
	verb := strings.TrimSpace(cmd)
	if i := strings.IndexAny(verb, " \t"); i > 0 {
		verb = verb[:i]
	}
	if verb != "" {
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
}

// execTerm 执行 DSL。
func (s *Server) execTerm(line string, v Viewer, c Caller) (TermResult, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		// 空命令返回总览。跟 termcp 一样：不知道该问什么的时候，
		// 空调用就是「我现在能做什么」这个问题的答案。
		return TermResult{Text: helpOverview()}, nil
	}
	parts := strings.Fields(line)
	verb := parts[0]
	rest := strings.TrimSpace(strings.TrimPrefix(line, verb))

	// persona 是写入与 branch/tag 类动词的目标分支线。
	//
	// 唯一来源是服务端的 s.persona（serve -profile 或配置的默认值）。
	// 曾经还可以从 ?profile= 逐请求覆盖，那条路已经删了：
	// 它让「这条记忆属于谁」看起来是请求属性，而真相是分支。
	persona := s.persona
	if persona == "" {
		persona = "zero"
	}

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
		return TermResult{}, fmt.Errorf("没有 %q 这个动词\n\n%s", strings.Fields(arg)[0], helpOverview())

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
		// 抛弃，等于让其他角色的内容占掉名额——最终结果可能是
		// 「明明有 360 篇相关记录，recall 返回无命中」。
		// 在角色的世界里算出 top-N，才是它该看到的最佳答案。
		allowedHit := func(addr string) bool {
			proj, br, _, ok := memory.SplitID(addr)
			if !ok {
				proj = s.projectOfAddr(addr)
				br = s.persona
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
		hits := s.idx.SearchWeightedFiltered(query, 3, 0, s.weightCount, func(addr string) bool {
			return allowedHit(addr) && scopeMatch(addr)
		})
		permHits := make([]index.Hit, 0, len(hits))
		for _, h := range hits {
			// 命中项的地址形如 project@branch/path——
			// 必须用它自己的分支判权限，不能用 s.persona。
			//
			// 踩过的坑：早先一律按 s.persona（服务端人格分支）判定，
			// 于是 chat 角色写进 daily@chat 的内容虽然进了索引，
			// 却因为「chat 挂载表里没有 zero 分支」被全部过滤掉——
			// 症状是 recall 永远「无命中」，而内容明明写成功了。
			proj, br, _, ok := memory.SplitID(h.Addr)
			if !ok {
				proj = s.projectOfAddr(h.Addr)
				br = s.persona
			}
			if s.allowed(c, proj, br, configBitRead) {
				permHits = append(permHits, h)
			}
		}
		hits = permHits
		s.mu.RUnlock()
		if len(hits) == 0 {
			return TermResult{
				Text:       "无命中",
				Structured: map[string]any{"hits": []any{}, "count": 0},
			}, nil
		}
		var sb strings.Builder
		structHits := make([]any, 0, len(hits))
		for _, h := range hits {
			_, hbr, _, okBr := memory.SplitID(h.Addr)
			if !okBr {
				hbr = s.persona
			}
			virt := s.addrToVirtual(h.Addr, hbr, c)
			if virt == "" {
				continue
			}
			sb.WriteString(fmt.Sprintf("%s (%.2f)\n", virt, h.Score))
			snip := s.snippet(h.Addr, query, 240)
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
			return TermResult{}, errors.New("read 语法: read <addr> [<addr> ...]")
		}
		s.mu.RLock()
		defer s.mu.RUnlock()

		var out strings.Builder
		missing := []string{}
		for _, addr := range fields {
			proj, br, path, err := s.resolveAddr(addr, c)
			if err == nil && !s.allowed(c, proj, br, configBitRead) {
				out.WriteString("read: " + addr + " → " + s.denyReason(c, proj, br, configBitRead) + "\n\n")
				continue
			}
			if err != nil {
				missing = append(missing, addr+"("+err.Error()+")")
				continue
			}
			repo, err := s.projects.Get(proj)
			if err != nil {
				missing = append(missing, addr+"(项目不存在)")
				continue
			}
			d, err := repo.Read(br, path)
			if err != nil {
				missing = append(missing, addr+"(not found)")
				continue
			}
			// 权重键必须与索引键一致（带分支），否则 read 强化对
			// 不带分支的地址记账、索引却按带分支的键查，永远对不上。
			nowUnix := time.Now().Unix()
			s.weights.Bump(proj+"@"+br+"/"+path, nowUnix)
			s.weights.Bump(proj+"/"+path, nowUnix)
			if len(fields) > 1 {
				out.WriteString("════ " + addr + " ════\n")
			}
			out.WriteString(string(d))
			out.WriteString("\n")
		}
		if out.Len() == 0 {
			return TermResult{}, fmt.Errorf("not found: %s", strings.Join(missing, ", "))
		}
		if len(missing) > 0 {
			out.WriteString("\n[未取到] " + strings.Join(missing, ", ") + "\n")
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

	case "push":
		if !c.canWrite() {
			return TermResult{}, fmt.Errorf("推送需要 w 能力")
		}
		return s.execPush(rest, c)

	case "sync":
		return s.execSync(rest, c)

	case "branch":
		// branch [<project> [<new> [from <src>]]]
		out, err := s.execBranch(rest, c, persona)
		return TermResult{Text: out}, err

	case "tag":
		// tag [-d] <project> [<name> [<ref>]]
		out, err := s.execTag(rest, c, persona)
		return TermResult{Text: out}, err

	case "cp":
		// cp <src>@<br>/<path> ... -> <dst>@<br>/[<path>]
		out, err := s.execCrossBranch(rest, c, false)
		return TermResult{Text: out}, err

	case "mv":
		// mv <src>@<br>/<path> ... -> <dst>@<br>/[<path>]
		out, err := s.execCrossBranch(rest, c, true)
		return TermResult{Text: out}, err

	case "chmod":
		// chmod <project>[@<branch>]/<path> rule|content
		out, err := s.execChmod(rest, c, persona)
		return TermResult{Text: out}, err

	case "delete":
		// delete <project>@<branch>/<path> [...]
		out, err := s.execDelete(rest, c, persona)
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
		// archive <project>@<branch> [-> <project>@<as>]
		out, err := s.execArchive(rest, c)
		return TermResult{Text: out}, err

	case "reset":
		// reset <project>@<branch> from <project>@<src>
		out, err := s.execReset(rest, c)
		return TermResult{Text: out}, err

	case "batch":
		// batch {"ops":[{...}]}
		out, err := s.execBatch(rest, c, persona)
		return TermResult{Text: out}, err

	case "diff":
		// diff <project>@<branch>  或  diff a@br -> b@br
		out, err := s.execDiff(rest, c, persona)
		return TermResult{Text: out}, err

	case "commit":
		// commit [<project>[@<branch>]] [-m <msg>]
		out, err := s.execCommit(rest, c, persona)
		return TermResult{Text: out}, err

	case "write":
		// write <project>[@<branch>]/<path> -- <内容>
		out, err := s.execWrite(rest, c, persona)
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
		return TermResult{Text: "索引已重建"}, nil

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
		s.mu.RLock()
		docs := 0
		for _, t := range s.trees {
			if !s.allowed(c, t.Project, t.Branch, configBitRead) {
				continue
			}
			docs += len(t.Indexable())
		}
		terms := s.idx.Terms()
		s.mu.RUnlock()
		return TermResult{
			Text: fmt.Sprintf("docs: %d, terms: %d", docs, terms),
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
		pattern, scope := splitScope(rest)
		if pattern == "" {
			return TermResult{}, errors.New("grep 语法: grep <pattern> [--scope <tree>]")
		}
		grepText, grepErr := s.execGrep(pattern, scope, c)
		if grepErr != nil {
			return TermResult{}, grepErr
		}
		return TermResult{Text: grepText}, nil

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
			return TermResult{}, fmt.Errorf("没有 %q 这个动词；你是不是想输入 %q？", verb, suggestion)
		}
		return TermResult{}, fmt.Errorf("没有 %q 这个动词（若对接了下游 MCP，此处应透传）\n\n%s", verb, helpOverview())
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
			tag := "索引"
			if b.Injectable() {
				tag = "注入"
			}
			if v := c.Mount.ToVirtual(b.Project, b.Branch, b.Path); v != "" {
				fmt.Fprintf(w, "  [%s] %7d  %s\n", tag, b.Size, v)
			}
		}
		st := t.Stats()
		fmt.Fprintf(w, "  --- %d 块 / 规则 %d 块 / 索引 %d 字节\n",
			st.Blocks, st.Rules, st.IndexBytes)
	}
	fmt.Fprintf(w, "\n索引：%d 块 / %d 词\n", s.idx.Docs(), s.idx.Terms())
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
		return string(r) + "…"
	}
	return string(r)
}

func (s *Server) weightCount(addr string) int {
	if s.weights == nil {
		return 0
	}
	n := s.weights.Count(addr)
	if n > 0 {
		return n
	}
	// 如果 addr 是 project@branch/path，也查一下无分支的 project/path 兜底
	if p, _, path, ok := memory.SplitID(addr); ok {
		return s.weights.Count(p + "/" + path)
	}
	return 0
}

// execGrep 在所有可 recall 的块里正则搜索，返回命中行及其上下文。
//
// scope 非空时只搜该范围下的条目。范围可以写成 project、
// project@branch 或 project@branch/subtree，与 recall 的 --scope 同一套语义。
//
// 为什么要先剥参数再编译正则：早先整个 rest 直接当正则用，
// 于是 `grep 1729 --scope daily` 会把 "--scope daily" 也编进模式里，
// 结果是必然零命中——而报错文本是「无命中」，看上去像内容不存在。
func (s *Server) execGrep(pattern, scope string, c Caller) (string, error) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return "", fmt.Errorf("非法正则: %w", err)
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
			if scope != "" && !addrUnderScope(b.Project+"@"+b.Branch+"/"+b.Path, scope) {
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
						sb.WriteString("── " + c.Mount.ToVirtual(b.Project, b.Branch, b.Path) + " ──\n")
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
						sb.WriteString("\n[已截断：命中过多，只显示前 30 处]\n")
						return sb.String(), nil
					}
				}
			}
			if blockHit {
				sb.WriteString("\n")
			}
		}
	}
	if totalHits == 0 {
		return "无命中", nil
	}
	return strings.TrimRight(sb.String(), "\n"), nil
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
	// 按 ModTime 倒序，平局按地址倒序
	sort.Slice(all, func(i, j int) bool {
		if all[i].Time != all[j].Time {
			return all[i].Time > all[j].Time
		}
		return all[i].Path > all[j].Path
	})
	if len(all) > n {
		all = all[:n]
	}

	var sb strings.Builder
	for i, b := range all {
		t := time.Unix(b.Time, 0).Format("2006-01-02 15:04:05")
		virt := c.Mount.ToVirtual(b.Project, b.Branch, b.Path)
		if virt == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%d] %s (%s, %d 字节)\n", i+1, virt, t, b.Size))
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
func (s *Server) execLs(day string, c Caller) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

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
			if day == "" || strings.Contains(virt, day) {
				matches = append(matches, b)
			}
		}
	}
	if len(matches) == 0 {
		return "无匹配切片", nil
	}
	sort.Slice(matches, func(i, j int) bool {
		return c.Mount.ToVirtual(matches[i].Project, matches[i].Branch, matches[i].Path) <
			c.Mount.ToVirtual(matches[j].Project, matches[j].Branch, matches[j].Path)
	})

	var sb strings.Builder
	for _, b := range matches {
		virt := c.Mount.ToVirtual(b.Project, b.Branch, b.Path)
		if virt == "" {
			virt = b.Path
		}
		sb.WriteString(fmt.Sprintf("%-50s %6d 字节\n", virt, b.Size))
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// handleProfileSub 处理 /profiles/<name>/ensure。

// EnsureProfile 确保某份身份存在。
//
// 如果已存在：幂等返回。
// 如果不存在且有 MemoriesRoot：从模板或内建骨架创建，然后重扫。
// 没有 MemoriesRoot 时报错——单身份模式不支持运行时加身份。

// provisionTemplate 填充一个新身份目录的骨架。

// copyDir 递归复制目录

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
// 这是给内嵌调用者使用的快捷入口：它用 root 权限、默认人格、
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
	return s.muxFor(ListenBlock{Allow: []string{"*"}})
}

// ReposMux 只暴露 HTTP Git 端点（/repos/）。
func (s *Server) ReposMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", s.handleGitHTTP)
	return mux
}
