package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// File 是整份配置的语义形态。
type File struct {
	Server   Server
	Commit   Commit
	GC       GC
	Semantic Semantic
	Users    []User
	Groups   []Group
	Listens  []ListenBlock
	Projects []Project
	Access   []AccessRule
	Remotes  []Remote

	// Path 是它被读出来的位置，仅用于错误信息与日志。
	Path string
}

// Semantic 是本地语义向量检索配置。
type Semantic struct {
	Lib     string // ONNX Runtime 动态库路径（dylib/so）
	Model   string // int8 量化 ONNX 模型路径
	Vocab   string // BERT 词表路径
	Vectors string // 向量缓存落盘目录，空则在 data/vectors

	// Providers 是降级链，按优先级排列。非空时优先于上面的单后端字段。
	//
	// 为什么要链而不是单一后端：目标环境是「3060 主机跑 GPU 推理 +
	// Mac 本地兜底」。3060 会断网、会休眠、会重启；每次都让人手工改
	// 配置不现实。链上第一个不可用就降到下一个，粘性降级避免每次
	// 请求都先吃一次超时。
	//
	// 硬约束：链上所有后端必须同模型同维度。不同模型的向量不可比，
	// 混在一个索引里检索会直接失效。加载时会校验。
	Providers []SemanticProvider

	// BatchSize 是建索引时一次请求的文本条数。0 表示用默认值（32）。
	// HTTP 后端靠它吃到批处理吞吐，ONNX 后端忽略。
	BatchSize int
}

// SemanticProvider 是降级链上的一个向量化后端。
type SemanticProvider struct {
	// Label 是诊断用的名字（"3060"、"local"），出现在日志与 status。
	Label string
	// Kind 是 "http" 或 "onnx"。
	Kind string
	// Endpoint 是 HTTP 后端的基地址。
	Endpoint string
	// Model 是模型标识。同一条链上必须一致。
	Model string
	// Dim 是向量维度。同一条链上必须一致；0 表示由首次响应推断。
	Dim int
	// APIKey 是 Bearer token（可选）。
	APIKey string
	// Timeout 是单次请求超时。远端应设短，让断网快速降级。
	Timeout time.Duration
	// ONNX 后端专用路径（不填则继承 [semantic] 的 lib/model/vocab）。
	LibPath   string
	ModelPath string
	VocabPath string
}

// Server 是监听与身份。
type Server struct {
	Data     string
	Listen   string
	Endpoint string
	// DefaultBranch 是默认分支名。空表示用内置缺省（zero）。
	//
	// 分支是「这份记忆的版本线」，不是「谁在用」——
	// 所以它是一个存储参数，而不是身份参数。
	DefaultBranch string
	// EnforceAuth 为真时，即使是回环地址也强制要求凭证。
	EnforceAuth bool
}

// ListenBlock 是一个独立的监听端口及其安全策略。
//
// 为什么要把权限放在端口上：
//
//	端口是内核级的边界，比代码里的 if 坚固得多。
//	一个端口如果不配写动词，它的路由里根本没有写代码；
//	一个端口如果挂载了某个分支，它物理上看不见其他分支。
type ListenBlock struct {
	Addr    string
	Allow   []string
	Require string
	// Mount 格式与 token 的 mount 一致，端口级兜底收窄
	Mount []MountSpec
	// WebUI 为真时，这个端口才注册管理面路由，默认关闭。
	WebUI bool
}

type MountSpec struct {
	Project string
	At      string
	Branch  string
	Cap     string
}

// Commit 控制攒批提交。
//
// 为什么不每条写入都 commit：一条记忆一个 commit 会让对象数变成五倍，
// 而且两台机器各自写就各自分叉，合并粒度从「一批」变成「一个点」。
type Commit struct {
	// Interval 是最大等待时间。0 表示不按时间触发。
	Interval time.Duration
	// Count 是最大积累条数。0 表示不按条数触发。
	Count int
}

// GC 控制打包。
//
// 不做 gc 的后果不是磁盘满，是 inode 先耗尽——而 inode 耗尽的表现
// 是「磁盘还有空间但什么都写不进去」，排查起来毫无头绪。
type GC struct {
	Interval time.Duration
}

// User 是一个可以持有凭证的身份。
type User struct {
	Name   string
	Groups []string
	// Root 为真时绕过所有权限检查。对应 POSIX 的 uid 0。
	Root bool
	// Pwd 是这个用户的默认工作目录（项目名）。
	//
	// 设了它，该用户就能用相对路径读写（"2026-10-30/x.md"），
	// 服务端自动补成 "<pwd>/2026-10-30/x.md"。
	// 空表示必须写全路径——这是安全的缺省：不给默认值就不会误写别处。
	Pwd string
}

// Group 是一组用户，用于项目权限里的「属组」那三位。
type Group struct {
	Name    string
	Members []string
}

// Project 是一个裸仓库。Name 就是 <data>/repos/<name>.git 的目录名。
type Project struct {
	Name string
	// Owner 是属主用户名。空表示无主（只有 root 和其他位生效）。
	Owner string
	// Group 是属组名。空表示无组。
	Group string
	// Mode 是九字符或八进制权限位。
	Perm auth.Perm
}

// Remote 是一个项目记忆仓库的远端。
//
// 它只声明「能去哪」和「能推什么」，不含任何凭证：
// url 里内嵌密码会让它出现在 TOML、日志和错误信息里三处。
type Remote struct {
	// Project 是本地项目名，例如 blackegg/me。
	Project string
	// Name 是远端别名，例如 origin / github / blackbox。
	Name string
	// URL 是远端地址，禁止内嵌凭证。
	URL string
	// FetchOnly 为真时禁止推送。
	FetchOnly bool
	// Push 是允许推送的分支白名单；空表示不推任何分支。
	Push []string
	// SSHKey 是服务端 system/sshkeys 下的私钥名；不包含私钥内容。
	SSHKey string
	// HostKey 是 SSH server 的 SHA256 host-key fingerprint，必须显式 pin。
	HostKey string
	// HTTPUser 是 HTTP Basic 的用户名（GitHub 用 x-access-token）。
	HTTPUser string
	// HTTPToken 是服务端密钥库里的 token 名，不是 token 本身。
	HTTPToken string
}

// AccessRule 是绕过项目 mode 的一条例外授权。
//
// 它的用处是「给某个用户单独开某个项目的口子」，而不必为此改项目归属
// 或者把人塞进组里。优先级高于 mode。
type AccessRule struct {
	User    string
	Project string
	Perm    string // rwx 的子集
}

// Default 是要么全默认、要么全报错的那一组缺省值。
func Default() File {
	return File{
		Server: Server{Listen: "127.0.0.1:1729"},
		Commit: Commit{Interval: 30 * time.Second, Count: 50},
		GC:     GC{Interval: 24 * time.Hour},
	}
}

// Load 读并校验一份配置文件。
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	f, err := ParseFile(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.Path = path
	return f, nil
}

// 每个分节允许的键。写错一个键名不能被静默忽略——
// 那会变成「配置里写了但没生效」，而症状是权限不对、不是报错。
var knownKeys = map[string]map[string]bool{
	"server":   {"data": true, "listen": true, "endpoint": true, "default_branch": true, "enforce_auth": true},
	"semantic": {"lib": true, "model": true, "vocab": true, "vectors": true, "batch_size": true},
	"semantic.provider": {
		"label": true, "kind": true, "endpoint": true, "model": true,
		"dim": true, "api_key": true, "timeout": true,
		"lib": true, "model_path": true, "vocab": true,
	},
	"commit":   {"interval": true, "count": true},
	"gc":      {"interval": true},
	"user":    {"name": true, "groups": true, "root": true, "pwd": true},
	"group":   {"name": true, "members": true},
	"project": {"name": true, "owner": true, "group": true, "mode": true},
	"access":  {"user": true, "project": true, "perm": true},
	"listen":  {"addr": true, "allow": true, "require": true, "mount": true, "webui": true},
	"remote":  {"project": true, "name": true, "url": true, "fetch_only": true, "push": true, "ssh_key": true, "host_key": true, "http_user": true, "http_token": true},
}

// checkKeys 拒绝未知分节与未知键。
func checkKeys(doc *Document) error {
	for name, sec := range doc.Sections {
		allowed, ok := knownKeys[name]
		if !ok {
			return fmt.Errorf("unknown section [%s] (typo?)", name)
		}
		for k, v := range sec {
			if !allowed[k] {
				return fmt.Errorf("unknown key %q in [%s] (line %d)", name, k, v.Line)
			}
		}
	}
	for name, list := range doc.Arrays {
		allowed, ok := knownKeys[name]
		if !ok {
			return fmt.Errorf("unknown array table [[%s]] (typo?)", name)
		}
		for i, sec := range list {
			for k, v := range sec {
				if !allowed[k] {
					return fmt.Errorf("unknown key %q in [[%s]] #%d (line %d)", k, name, i+1, v.Line)
				}
			}
		}
	}
	return nil
}

// ParseFile 解析配置内容。
func ParseFile(raw []byte) (*File, error) {
	doc, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := checkKeys(doc); err != nil {
		return nil, err
	}

	f := Default()

	if sec, ok := doc.Sections["server"]; ok {
		if v, err := str(sec, "server", "data"); err != nil {
			return nil, err
		} else if v != "" {
			// "~/..." is the natural thing to write in a config file, and the
			// example config uses it. Expanding only the -data flag meant a
			// config-supplied path created a literal directory named "~" in the
			// current working directory - data written somewhere nobody looks,
			// and a stray directory in the repo.
			f.Server.Data = ExpandHome(v)
		}
		if v, err := str(sec, "server", "listen"); err != nil {
			return nil, err
		} else if v != "" {
			f.Server.Listen = v
		}
		if v, err := str(sec, "server", "endpoint"); err != nil {
			return nil, err
		} else if v != "" {
			f.Server.Endpoint = v
		}
		if v, err := str(sec, "server", "default_branch"); err != nil {
			return nil, err
		} else if v != "" {
			f.Server.DefaultBranch = v
		}
		if v, err := boolean(sec, "server", "enforce_auth"); err != nil {
			return nil, err
		} else {
			f.Server.EnforceAuth = v
		}
	}

	if sec, ok := doc.Sections["semantic"]; ok {
		if v, err := str(sec, "semantic", "lib"); err != nil {
			return nil, err
		} else {
			f.Semantic.Lib = v
		}
		if v, err := str(sec, "semantic", "model"); err != nil {
			return nil, err
		} else {
			f.Semantic.Model = v
		}
		if v, err := str(sec, "semantic", "vocab"); err != nil {
			return nil, err
		} else {
			f.Semantic.Vocab = v
		}
		if v, err := str(sec, "semantic", "vectors"); err != nil {
			return nil, err
		} else {
			f.Semantic.Vectors = v
		}
		if v, err := integer(sec, "semantic", "batch_size"); err != nil {
			return nil, err
		} else if v != 0 {
			f.Semantic.BatchSize = int(v)
		}
	}

	// [[semantic.provider]] 降级链。
	// 注意：自研 TOML 解析器用 Arrays 存数组段，键名就是段名，
	// 所以 [[semantic.provider]] 以 "semantic.provider" 为键。
	for _, sec := range doc.Arrays["semantic.provider"] {
		var p SemanticProvider
		var err error
		const ctx = "[[semantic.provider]]"
		if p.Label, err = str(sec, ctx, "label"); err != nil {
			return nil, err
		}
		if p.Kind, err = str(sec, ctx, "kind"); err != nil {
			return nil, err
		}
		if p.Endpoint, err = str(sec, ctx, "endpoint"); err != nil {
			return nil, err
		}
		if p.Model, err = str(sec, ctx, "model"); err != nil {
			return nil, err
		}
		if v, err := integer(sec, ctx, "dim"); err != nil {
			return nil, err
		} else {
			p.Dim = int(v)
		}
		if p.APIKey, err = str(sec, ctx, "api_key"); err != nil {
			return nil, err
		}
		if v, err := dur(sec, ctx, "timeout"); err != nil {
			return nil, err
		} else {
			p.Timeout = v
		}
		if p.LibPath, err = str(sec, ctx, "lib"); err != nil {
			return nil, err
		}
		if p.ModelPath, err = str(sec, ctx, "model_path"); err != nil {
			return nil, err
		}
		if p.VocabPath, err = str(sec, ctx, "vocab"); err != nil {
			return nil, err
		}
		if p.Kind == "" {
			p.Kind = "http"
		}
		if p.Kind != "http" && p.Kind != "onnx" {
			return nil, fmt.Errorf("[[semantic.provider]] kind: want http or onnx, got %q", p.Kind)
		}
		if p.Kind == "http" && p.Endpoint == "" {
			return nil, fmt.Errorf("[[semantic.provider]] %q: http provider requires endpoint", p.Label)
		}
		f.Semantic.Providers = append(f.Semantic.Providers, p)
	}

	if sec, ok := doc.Sections["commit"]; ok {
		if v, err := dur(sec, "commit", "interval"); err != nil {
			return nil, err
		} else if v != 0 {
			f.Commit.Interval = v
		}
		if v, err := integer(sec, "commit", "count"); err != nil {
			return nil, err
		} else if v != 0 {
			f.Commit.Count = int(v)
		}
	}

	for _, sec := range doc.Arrays["remote"] {
		var rm Remote
		var err error
		if rm.Project, err = str(sec, "[[remote]]", "project"); err != nil {
			return nil, err
		}
		if rm.Name, err = str(sec, "[[remote]]", "name"); err != nil {
			return nil, err
		}
		if rm.URL, err = str(sec, "[[remote]]", "url"); err != nil {
			return nil, err
		}
		if rm.FetchOnly, err = boolean(sec, "[[remote]]", "fetch_only"); err != nil {
			return nil, err
		}
		if rm.Push, err = list(sec, "[[remote]]", "push"); err != nil {
			return nil, err
		}
		if rm.SSHKey, err = str(sec, "[[remote]]", "ssh_key"); err != nil {
			return nil, err
		}
		if rm.HostKey, err = str(sec, "[[remote]]", "host_key"); err != nil {
			return nil, err
		}
		if rm.SSHKey != "" {
			if err := ValidateSSHKeyName(rm.SSHKey); err != nil {
				return nil, fmt.Errorf("[[remote]] ssh_key: %w", err)
			}
		}
		if rm.HostKey != "" && !strings.HasPrefix(rm.HostKey, "SHA256:") {
			return nil, fmt.Errorf("[[remote]] host_key must be a SHA256:... fingerprint")
		}
		if rm.HTTPUser, err = str(sec, "[[remote]]", "http_user"); err != nil {
			return nil, err
		}
		if rm.HTTPToken, err = str(sec, "[[remote]]", "http_token"); err != nil {
			return nil, err
		}
		if rm.HTTPToken != "" {
			if err := ValidateSSHKeyName(rm.HTTPToken); err != nil {
				return nil, fmt.Errorf("[[remote]] http_token: %w", err)
			}
		}
		// 凭证名与传输方式必须对得上：ssh_key 配 https、http_token 配 ssh://
		// 都是「配了却没生效」的典型来源，只有启动期报错才治得住。
		if rm.SSHKey != "" && (strings.HasPrefix(rm.URL, "http://") || strings.HasPrefix(rm.URL, "https://")) {
			return nil, fmt.Errorf("[[remote]] %s/%s: ssh_key cannot be used with an HTTP remote; use http_token", rm.Project, rm.Name)
		}
		if rm.HTTPToken != "" && !strings.HasPrefix(rm.URL, "http://") && !strings.HasPrefix(rm.URL, "https://") {
			return nil, fmt.Errorf("[[remote]] %s/%s: http_token only applies to http(s) remotes", rm.Project, rm.Name)
		}
		if rm.Project == "" || rm.Name == "" || rm.URL == "" {
			return nil, fmt.Errorf("[[remote]] requires project, name and url")
		}
		if err := validateRemoteURL(rm.URL); err != nil {
			return nil, fmt.Errorf("[[remote]] %s/%s: %w", rm.Project, rm.Name, err)
		}
		f.Remotes = append(f.Remotes, rm)
	}

	if sec, ok := doc.Sections["gc"]; ok {
		if v, err := dur(sec, "gc", "interval"); err != nil {
			return nil, err
		} else if v >= 0 {
			f.GC.Interval = v
		}
	}

	for _, sec := range doc.Arrays["user"] {
		u := User{}
		if u.Name, err = str(sec, "[[user]]", "name"); err != nil {
			return nil, err
		}
		if u.Groups, err = list(sec, "[[user]]", "groups"); err != nil {
			return nil, err
		}
		if u.Root, err = boolean(sec, "[[user]]", "root"); err != nil {
			return nil, err
		}
		if u.Pwd, err = str(sec, "[[user]]", "pwd"); err != nil {
			return nil, err
		}
		f.Users = append(f.Users, u)
	}

	for _, sec := range doc.Arrays["group"] {
		g := Group{}
		if g.Name, err = str(sec, "[[group]]", "name"); err != nil {
			return nil, err
		}
		if g.Members, err = list(sec, "[[group]]", "members"); err != nil {
			return nil, err
		}
		f.Groups = append(f.Groups, g)
	}

	for i, sec := range doc.Arrays["project"] {
		p := Project{}
		if p.Name, err = str(sec, "[[project]]", "name"); err != nil {
			return nil, err
		}
		if p.Owner, err = str(sec, "[[project]]", "owner"); err != nil {
			return nil, err
		}
		if p.Group, err = str(sec, "[[project]]", "group"); err != nil {
			return nil, err
		}
		mode, err := str(sec, "[[project]]", "mode")
		if err != nil {
			return nil, err
		}
		if mode == "" {
			// 缺省 0755：主可读写执行、同组可读可执行、其他可读。
			// 这是「默认别人能看但不能改」，比默认 0777 安全，
			// 比默认 0700 少一次「为什么别人读不到」的排查。
			mode = "0755"
		}
		p.Perm, err = auth.ParsePerm(mode)
		if err != nil {
			return nil, fmt.Errorf("[[project]] #%d mode: %w", i+1, err)
		}
		f.Projects = append(f.Projects, p)
	}

	for _, sec := range doc.Arrays["listen"] {
		var lb ListenBlock
		if v, err := str(sec, "[[listen]]", "addr"); err != nil {
			return nil, err
		} else {
			lb.Addr = v
		}
		if v, err := str(sec, "[[listen]]", "require"); err != nil {
			return nil, err
		} else {
			lb.Require = v
		}
		if v, err := list(sec, "[[listen]]", "allow"); err != nil {
			return nil, err
		} else {
			lb.Allow = v
		}
		if v, err := list(sec, "[[listen]]", "mount"); err != nil {
			return nil, err
		} else {
			for _, item := range v {
				lb.Mount = append(lb.Mount, parseMountSpec(item))
			}
		}
		if v, err := boolean(sec, "[[listen]]", "webui"); err != nil {
			return nil, err
		} else {
			lb.WebUI = v
		}
		// 没配 addr 的块直接忽略：宁可少开一个端口，也不要开在意外的地方
		if lb.Addr == "" {
			continue
		}
		f.Listens = append(f.Listens, lb)
	}

	for _, sec := range doc.Arrays["access"] {
		a := AccessRule{}
		if a.User, err = str(sec, "[[access]]", "user"); err != nil {
			return nil, err
		}
		if a.Project, err = str(sec, "[[access]]", "project"); err != nil {
			return nil, err
		}
		if a.Perm, err = str(sec, "[[access]]", "perm"); err != nil {
			return nil, err
		}
		f.Access = append(f.Access, a)
	}

	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate 校验内部一致性。
//
// 全部在启动前做完：配置错误在运行中才暴露出来时，表现往往是
// 「某个身份忽然什么都没有权限」，而不是一条清晰的错误。
func (f *File) Validate() error {
	if len(f.Users) == 0 {
		return fmt.Errorf("at least one [[user]] is required (otherwise nobody can hold a credential)")
	}

	userNames := map[string]bool{}
	for i, u := range f.Users {
		if err := auth.ValidateName(u.Name); err != nil {
			return fmt.Errorf("[[user]] #%d name: %w", i+1, err)
		}
		if userNames[u.Name] {
			return fmt.Errorf("duplicate user %q", u.Name)
		}
		userNames[u.Name] = true
	}

	groupNames := map[string]bool{}
	for i, g := range f.Groups {
		if err := auth.ValidateName(g.Name); err != nil {
			return fmt.Errorf("[[group]] #%d name: %w", i+1, err)
		}
		if groupNames[g.Name] {
			return fmt.Errorf("duplicate group %q", g.Name)
		}
		groupNames[g.Name] = true
	}

	// 用户声明的组必须存在。静默忽略一个拼错的组名，结果是
	// 这个用户对某个项目忽然失去权限，而且没有任何提示。
	for _, u := range f.Users {
		for _, g := range u.Groups {
			if !groupNames[g] {
				return fmt.Errorf("user %q references undeclared group %q", u.Name, g)
			}
		}
	}
	for _, g := range f.Groups {
		for _, m := range g.Members {
			if !userNames[m] {
				return fmt.Errorf("group %q member %q is not a declared user", g.Name, m)
			}
		}
	}

	projNames := map[string]bool{}
	for i, p := range f.Projects {
		if err := auth.ValidateRepoName(p.Name); err != nil {
			return fmt.Errorf("[[project]] #%d name: %w", i+1, err)
		}
		if projNames[p.Name] {
			return fmt.Errorf("duplicate project %q", p.Name)
		}
		projNames[p.Name] = true

		if p.Owner != "" && !userNames[p.Owner] {
			return fmt.Errorf("project %q owner %q is not a declared user", p.Name, p.Owner)
		}
		if p.Group != "" && !groupNames[p.Group] {
			return fmt.Errorf("project %q group %q is not a declared group", p.Name, p.Group)
		}
	}

	// 用户的 pwd 必须是已声明的项目，否则相对路径会指向一个不存在的仓库，
	// 而症状是「明明写对了却 not found」。
	for _, u := range f.Users {
		if u.Pwd != "" && !projNames[u.Pwd] {
			return fmt.Errorf("user %q pwd %q is not a declared project", u.Name, u.Pwd)
		}
	}

	for i, a := range f.Access {
		if !userNames[a.User] {
			return fmt.Errorf("[[access]] #%d user %q is not a declared user", i+1, a.User)
		}
		if !projNames[a.Project] {
			return fmt.Errorf("[[access]] #%d project %q is not a declared project", i+1, a.Project)
		}
		if a.Perm == "" {
			return fmt.Errorf("[[access]] #%d is missing perm", i+1)
		}
		for _, c := range a.Perm {
			if !strings.ContainsRune("rwx", c) {
				return fmt.Errorf("[[access]] #%d perm %q may only contain rwx", i+1, a.Perm)
			}
		}
	}

	// 端口级挂载项同样要校验。
	//
	// 这一层以前完全不查：写错的项目名、写错的 cap 字母、写错的
	// 虚拟路径都会静默变成一个「永远不匹配」的挂载项，而症状是
	// 「这个端口什么都看不到」——一个需要读完 portpolicy.go 才能
	// 解释的现象。端口策略是安全边界，它的配置就该在启动时校验。
	seenAddr := map[string]bool{}
	for i, lb := range f.Listens {
		if lb.Addr == "" {
			continue
		}
		if seenAddr[lb.Addr] {
			return fmt.Errorf("duplicate [[listen]] addr %q", lb.Addr)
		}
		seenAddr[lb.Addr] = true
		// An empty allow list must be rejected, not guessed.
		//
		// The two consumers disagreed: the verb layer treated an empty list as
		// "everything allowed" while the git layer treated it as "nothing
		// allowed". A config that forgot `allow` therefore produced a port with
		// every memory verb enabled - the opposite of what someone writing a
		// restricted listener intends. There is no safe default here, so demand
		// the list explicitly.
		if len(lb.Allow) == 0 {
			return fmt.Errorf("[[listen]] %q needs an allow list; write allow = [\"*\"] to enable everything", lb.Addr)
		}
		for j, m := range lb.Mount {
			if err := auth.ValidateRepoName(m.Project); err != nil {
				return fmt.Errorf("[[listen]] #%d mount #%d project name: %w", i+1, j+1, err)
			}
			if strings.ContainsRune(m.At, 0) || strings.ContainsAny(m.At, `\\:*?\"<>|`) {
				return fmt.Errorf("[[listen]] #%d mount #%d has an invalid virtual path: %q", i+1, j+1, m.At)
			}
			trimmedAt := strings.Trim(m.At, "/") // root "/" is valid
			if trimmedAt != "" {
				for _, seg := range strings.Split(trimmedAt, "/") {
					if seg == "" || seg == "." || seg == ".." {
						return fmt.Errorf("[[listen]] #%d mount #%d virtual path may not contain empty, . or .. segments: %q", i+1, j+1, m.At)
					}
				}
			}
			for _, c := range m.Cap {
				if !strings.ContainsRune("rwx", c) {
					return fmt.Errorf("[[listen]] #%d mount #%d cap %q may only contain rwx", i+1, j+1, m.Cap)
				}
			}
			if m.Branch != "" && m.Branch != "*" {
				if err := auth.ValidateBranchName(m.Branch); err != nil {
					return fmt.Errorf("[[listen]] #%d mount #%d branch name: %w", i+1, j+1, err)
				}
			}
		}
	}

	return nil
}

// Lookup 是 Validate 之后建好的索引，供运行时使用。
type Lookup struct {
	users    map[string]User
	groups   map[string]Group
	projects map[string]Project
	access   map[string]map[string]string // user -> project -> 权限字母
}

// Build 建立运行时索引。
func (f *File) Build() *Lookup {
	l := &Lookup{
		users:    map[string]User{},
		groups:   map[string]Group{},
		projects: map[string]Project{},
		access:   map[string]map[string]string{},
	}
	for _, u := range f.Users {
		l.users[u.Name] = u
	}
	for _, g := range f.Groups {
		l.groups[g.Name] = g
	}
	for _, p := range f.Projects {
		l.projects[p.Name] = p
	}
	for _, a := range f.Access {
		if l.access[a.User] == nil {
			l.access[a.User] = map[string]string{}
		}
		l.access[a.User][a.Project] = a.Perm
	}
	return l
}

// User 按名字取用户。
func (l *Lookup) User(name string) (User, bool) {
	u, ok := l.users[name]
	return u, ok
}

// Users 返回全部用户，按名字排序。
func (l *Lookup) Users() []User {
	out := make([]User, 0, len(l.users))
	for _, u := range l.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Project 按名字取项目。
func (l *Lookup) Project(name string) (Project, bool) {
	p, ok := l.projects[name]
	return p, ok
}

// Projects 返回全部项目，按名字排序。
func (l *Lookup) Projects() []Project {
	out := make([]Project, 0, len(l.projects))
	for _, p := range l.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GroupsOf 返回某个用户所属的全部组名。
//
// 两个来源取并集：user.groups 里写的，以及 group.members 里写的。
// 只认一处会让「我明明把他加进组了」变成一次无效排查。
func (l *Lookup) GroupsOf(user string) []string {
	seen := map[string]bool{}
	var out []string
	if u, ok := l.users[user]; ok {
		for _, g := range u.Groups {
			if !seen[g] {
				seen[g] = true
				out = append(out, g)
			}
		}
	}
	for name, g := range l.groups {
		for _, m := range g.Members {
			if m == user && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// AccessFor 返回某用户对某项目的显式授权（可能为空）。
func (l *Lookup) AccessFor(user, project string) (string, bool) {
	if m, ok := l.access[user]; ok {
		p, ok := m[project]
		return p, ok
	}
	return "", false
}

// ---------- 取值辅助：类型不对时报错而不是返回零值 ----------

func str(sec map[string]Value, section, key string) (string, error) {
	v, ok := sec[key]
	if !ok {
		return "", nil
	}
	if v.Kind != KindString {
		return "", fmt.Errorf("%s in %s must be a string (line %d)", section, key, v.Line)
	}
	return v.Str, nil
}

func list(sec map[string]Value, section, key string) ([]string, error) {
	v, ok := sec[key]
	if !ok {
		return nil, nil
	}
	if v.Kind != KindList {
		return nil, fmt.Errorf("%s in %s must be an array of strings (line %d)", section, key, v.Line)
	}
	return v.List, nil
}

func integer(sec map[string]Value, section, key string) (int64, error) {
	v, ok := sec[key]
	if !ok {
		return 0, nil
	}
	if v.Kind != KindInt {
		return 0, fmt.Errorf("%s in %s must be an integer (line %d)", section, key, v.Line)
	}
	return v.Int, nil
}

func boolean(sec map[string]Value, section, key string) (bool, error) {
	v, ok := sec[key]
	if !ok {
		return false, nil
	}
	if v.Kind != KindBool {
		return false, fmt.Errorf("%s in %s must be true/false (line %d)", section, key, v.Line)
	}
	return v.Bool, nil
}

func dur(sec map[string]Value, section, key string) (time.Duration, error) {
	s, err := str(sec, section, key)
	if err != nil || s == "" {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s in %s is not a valid duration (want 30s / 5m / 24h), got %q", section, key, s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s in %s must not be negative", section, key)
	}
	return d, nil
}

// parseMountSpec 解析端口级挂载项：project[=at[/:cap[@branch]]]
func parseMountSpec(raw string) MountSpec {
	m := MountSpec{Project: raw, At: "/"}
	if proj, rest, ok := strings.Cut(raw, "="); ok {
		m.Project = strings.TrimSpace(proj)
		at, capRest, _ := strings.Cut(rest, ":")
		m.At = strings.TrimSpace(at)
		if m.At == "" {
			m.At = "/"
		}
		capStr, branch, _ := strings.Cut(capRest, "@")
		m.Cap = strings.TrimSpace(capStr)
		m.Branch = strings.TrimSpace(branch)
	}
	return m
}

// EffectiveListens 返回实际要监听的端口列表。
//
// 没写 [[listen]] 时退回单 [server] listen，视作「一个全能力端口」。
// 这样老配置不用改，而新配置一旦出现 [[listen]] 就以它为准。
func (f *File) EffectiveListens() []ListenBlock {
	if len(f.Listens) > 0 {
		return f.Listens
	}
	addr := f.Server.Listen
	if addr == "" {
		addr = "127.0.0.1:1729"
	}
	return []ListenBlock{{Addr: addr, Allow: []string{"*"}}}
}

// Declared 报告项目是否在配置里显式声明。
//
// 与 Project 的区别只在语义：这个方法是「配置知道它吗」，
// 而 Project 还带着它的权限位。调用方想问的是前者时用这个，
// 读到零值权限位不会造成误判。
func (l *Lookup) Declared(name string) bool {
	_, ok := l.Project(name)
	return ok
}

// validateRemoteURL 拒绝内嵌凭证的远端地址。
//
// 与 gitremote.ValidateURL 同一条规则，但这里在配置解析期就执行：
// 一个带着密码的配置项如果能保存到磁盘，它就已经泄漏了。
func validateRemoteURL(url string) error {
	if strings.Contains(url, "@") {
		// git@host:path 是 SSH 用户名，不是密码；只有 scheme://user:pass@ 才是。
		if i := strings.Index(url, "://"); i >= 0 {
			rest := url[i+3:]
			if at := strings.Index(rest, "@"); at > 0 {
				userinfo := rest[:at]
				if strings.Contains(userinfo, ":") {
					return fmt.Errorf("remote URL must not embed a password (use a credential helper or SSH agent)")
				}
			}
		}
	}
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("remote address must not start with -")
	}
	return nil
}

// RemotesOf 返回某个项目声明的远端。
func (f *File) RemotesOf(project string) []Remote {
	var out []Remote
	for _, r := range f.Remotes {
		if r.Project == project {
			out = append(out, r)
		}
	}
	return out
}

// ValidateSSHKeyName accepts a short, path-safe identifier, never key material.
func ValidateSSHKeyName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("SSH key name must be 1..64 characters")
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || i > 0 && (r == '-' || r == '_' || r == '.')
		if !ok {
			return fmt.Errorf("SSH key name may only contain letters, digits and ._- : %q", name)
		}
	}
	return nil
}

// ExpandHome replaces a leading "~" with the user's home directory.
//
// Config files and command-line flags both go through this: a path like
// "~/.zero-memcp/data" is what people write, and leaving it unexpanded creates
// a directory literally named "~" in the working directory instead.
func ExpandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	rest := strings.TrimPrefix(p, "~")
	if rest == "" {
		return home
	}
	if !strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, string(filepath.Separator)) {
		// "~user/..." is not supported; leave it alone rather than guess.
		return p
	}
	return filepath.Join(home, rest)
}
