package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

func systemDir(data string) string { return filepath.Join(data, "system") }
func keyPath(data string) string   { return filepath.Join(systemDir(data), "jwt.key") }

// validateMountCap 校验一个挂载点的能力是否在凭证能力范围内。
//
// 空串在能力串里的含义是「不额外限制」，所以：
//   - 挂载点不写 cap：继承凭证级 cap，永远合法
//   - 凭证级不写 cap（= rwx）：任何挂载点 cap 都是它的子集
//   - 两边都写了：挂载点必须是子集
//
// 剩下的组合（两边都写了但不相交）在运行期会被算成空集，
// 而空集在该体系里另有含义（不额外限制）——签出这种凭证
// 等于把一个矛盾藏到权限判定里去。宁可现在拒绝。
func validateMountCap(credCap string, pt auth.MountPoint) error {
	if pt.Cap == "" {
		return nil
	}
	for _, ch := range pt.Cap {
		if !strings.ContainsRune("rwx", ch) {
			return fmt.Errorf("挂载点 %s 的 cap %q 只能由 rwx 组成", pt.Project, pt.Cap)
		}
	}
	if credCap == "" {
		return nil
	}
	for _, ch := range pt.Cap {
		if !strings.ContainsRune(credCap, ch) {
			return fmt.Errorf("挂载点 %s 的 cap %q 超出了凭证能力 %q（挂载点只能更窄，不能更宽）",
				pt.Project, pt.Cap, credCap)
		}
	}
	return nil
}

// runToken 分发 token 子命令。
func runToken(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法：zero-memcp token mint|list|revoke|unrevoke ...")
		return 2
	}
	switch args[0] {
	case "mint":
		return runTokenMint(args[1:])
	case "list", "ls":
		return runTokenList(args[1:])
	case "revoke":
		return runTokenRevoke(args[1:])
	case "unrevoke":
		return runTokenUnrevoke(args[1:])
	}
	fmt.Fprintf(os.Stderr, "zero-memcp: 未知的 token 子命令 %q\n", args[0])
	return 2
}

// runTokenMint 签发一张凭证。
//
// 凭证里只有身份，没有权限。权限永远以配置文件为准——
// 把权限塞进凭证的后果是改一次权限要重发所有 token，
// 而漏发一张就变成「为什么它还是旧权限」的悬案。
func runTokenMint(args []string) int {
	fs := flag.NewFlagSet("token mint", flag.ExitOnError)
	var (
		data   = fs.String("data", "", "数据根目录")
		cfgPth = fs.String("config", "", "配置文件（用来校验用户名是否存在）")
		name   = fs.String("name", "", "凭证名；缺省与 -user 相同")
		user   = fs.String("user", "", "使用者名，必须是配置里声明过的 [[user]]")
		exp    = fs.String("exp", "8760h", "有效期，例如 720h；0 或 never 表示永不过期")
		cap    = fs.String("cap", "", "能力上限（rwx 子集，x=规则注入，空表示不额外限制）")
		mounts = fs.String("mount", "", "挂载表，逗号分隔的 real=virt")
		parent = fs.String("parent", "", "父凭证 jti（用于级联吊销）")
		ticket = fs.Bool("ticket", false, "签发为一次性兑换券（不能直接读写记忆，只用于换取正式 token）")
	)
	_ = fs.Parse(args)

	dataDir := expandHome(*data)
	if dataDir == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 必须给 -data")
		return 2
	}
	if *user == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 必须给 -user（凭证代表谁）")
		return 2
	}
	sub := *user
	credName := *name
	if credName == "" {
		credName = sub
	}
	if err := auth.ValidateName(credName); err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 2
	}

	// 有配置文件就校验用户名，没有就跳过（单机模式）。
	var cfgFile *config.File
	if p := expandHome(*cfgPth); p != "" {
		var err error
		cfgFile, err = config.Load(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
			return 1
		}
		lookup := cfgFile.Build()
		if _, ok := lookup.User(sub); !ok {
			fmt.Fprintf(os.Stderr,
				"zero-memcp: 用户 %q 不在 %s 的 [[user]] 里。\n"+
					"  凭证只带身份，权限由配置决定——先把这个用户写进配置。\n", sub, p)
			return 2
		}
	}

	expAt, err := parseExpiry(*exp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 2
	}

	jti, err := auth.NewJTI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 1
	}

	secret, err := auth.LoadOrCreateSecret(keyPath(dataDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 1
	}

	var mountList []auth.MountPoint
	if *mounts != "" {
		for _, part := range strings.Split(*mounts, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			// 允许三种：
			//   "blackegg/me"
			//   "blackegg/me=/"
			//   "blackegg/me=/:rw"
			//   "blackegg/me=/:rw@chat"
			pt := auth.MountPoint{Project: part, At: "/"}
			if proj, rest, ok := strings.Cut(part, "="); ok {
				pt.Project = strings.TrimSpace(proj)
				at, capRest, _ := strings.Cut(rest, ":")
				pt.At = strings.TrimSpace(at)
				capStr, branch, _ := strings.Cut(capRest, "@")
				pt.Cap = strings.TrimSpace(capStr)
				pt.Branch = strings.TrimSpace(branch)
			}
			// 挂载点的能力必须是凭证能力的子集。
			//
			// 不校验的后果不是「多给了权限」——运行期取交集会把超出的
			// 部分删掉——而是「交集可能为空」，而空集在代码里另有含义
			// （不额外限制）。签一张 cap=r、挂载点 cap=w 的凭证，
			// 写权限的实际判定就变得依赖另一处的哨兵值，而不是这一处的
			// 声明。矛盾配置就该在签发时被拒，而不是留到运行期去猜。
			if err := validateMountCap(*cap, pt); err != nil {
				fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
				return 1
			}
			mountList = append(mountList, pt)
		}
	}

	// 父链记录是级联吊销的前提：父凭证一废，派生出的这张跟着废。
	if *parent != "" {
		rev := auth.NewRevocations(filepath.Join(systemDir(dataDir), "revoked"))
		if err := rev.RecordParent(jti, *parent); err != nil {
			fmt.Fprintf(os.Stderr, "zero-memcp: 记录父凭证失败: %v\n", err)
			return 1
		}
	}

	token, err := auth.NewTokenStore(secret).Sign(auth.Claims{
		Sub:    sub,
		Name:   credName,
		Exp:    expAt,
		JTI:    jti,
		Parent: *parent,
		Cap:    *cap,
		Mount:  mountList,
		Ticket: *ticket,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: 签发失败: %v\n", err)
		return 1
	}

	if err := auth.SaveTokenRecord(dataDir, auth.TokenRecord{
		Name:    credName,
		JTI:     jti,
		Sub:     sub,
		Exp:     expAt,
		Created: time.Now().Unix(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: 记录签发信息失败: %v\n", err)
		return 1
	}

	expText := "永不过期"
	if expAt > 0 {
		expText = time.Unix(expAt, 0).Format("2006-01-02 15:04")
	}

	fmt.Printf("凭证  %s\n", credName)
	fmt.Printf("身份  %s\n", sub)
	fmt.Printf("到期  %s\n", expText)
	fmt.Printf("JTI   %s\n", jti)
	fmt.Printf("\n%s\n", token)
	return 0
}

func parseExpiry(s string) (int64, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "never", "none":
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("非法有效期 %q（要 720h 这种，或 never）", s)
	}
	return time.Now().Add(d).Unix(), nil
}

func runTokenList(args []string) int {
	fs := flag.NewFlagSet("token list", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	_ = fs.Parse(args)

	dataDir := expandHome(*data)
	if dataDir == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 必须给 -data")
		return 2
	}

	recs, errs := auth.LoadTokenRecords(dataDir)
	rev := auth.NewRevocations(filepath.Join(systemDir(dataDir), "revoked"))
	revoked, _ := rev.List()

	inRevoked := map[string]bool{}
	for _, j := range revoked {
		inRevoked[j] = true
	}

	if len(recs) == 0 {
		fmt.Println("（还没有签发过凭证）")
	} else {
		now := time.Now()
		fmt.Printf("%-16s %-16s %-12s %-18s %s\n", "名字", "身份", "到期", "JTI", "状态")
		for _, r := range recs {
			exp := "永久"
			if r.Exp > 0 {
				exp = time.Unix(r.Exp, 0).Format("2006-01-02")
			}
			state := "有效"
			switch {
			case inRevoked[r.JTI]:
				state = "已吊销"
			case r.Expired(now):
				state = "已过期"
			}
			fmt.Printf("%-16s %-16s %-12s %-18s %s\n",
				r.Name, r.Sub, exp, r.JTI, state)
		}
	}

	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "  ! %v\n", e)
	}
	return 0
}

func runTokenRevoke(args []string) int {
	fs := flag.NewFlagSet("token revoke", flag.ExitOnError)
	var (
		data = fs.String("data", "", "数据根目录")
		jti  = fs.String("jti", "", "要吊销的凭证编号")
		name = fs.String("name", "", "按凭证名吊销（会自动查出它的 jti）")
	)
	_ = fs.Parse(args)

	dataDir := expandHome(*data)
	if dataDir == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 必须给 -data")
		return 2
	}

	target := *jti
	if target == "" && *name != "" {
		recs, _ := auth.LoadTokenRecords(dataDir)
		for _, r := range recs {
			if r.Name == *name {
				target = r.JTI
				break
			}
		}
		if target == "" {
			fmt.Fprintf(os.Stderr, "zero-memcp: 找不到名为 %q 的签发记录\n", *name)
			return 2
		}
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 要给 -jti 或 -name")
		return 2
	}

	rev := auth.NewRevocations(filepath.Join(systemDir(dataDir), "revoked"))
	if err := rev.Revoke(target); err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 1
	}
	fmt.Printf("已吊销 %s\n", target)
	return 0
}

func runTokenUnrevoke(args []string) int {
	fs := flag.NewFlagSet("token unrevoke", flag.ExitOnError)
	var (
		data = fs.String("data", "", "数据根目录")
		jti  = fs.String("jti", "", "要恢复的凭证编号")
	)
	_ = fs.Parse(args)

	dataDir := expandHome(*data)
	if dataDir == "" || *jti == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 要给 -data 与 -jti")
		return 2
	}

	rev := auth.NewRevocations(filepath.Join(systemDir(dataDir), "revoked"))
	if err := rev.Unrevoke(*jti); err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 1
	}
	fmt.Printf("已恢复 %s\n", *jti)
	return 0
}

// runKeygen 只生成密钥，不启动服务。
func runKeygen(args []string) int {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	force := fs.Bool("force", false, "已存在时重新生成（会让所有已签发凭证立即失效）")
	_ = fs.Parse(args)

	dataDir := expandHome(*data)
	if dataDir == "" {
		fmt.Fprintln(os.Stderr, "zero-memcp: 必须给 -data")
		return 2
	}
	path := keyPath(dataDir)

	if _, err := os.Stat(path); err == nil {
		if !*force {
			fmt.Fprintf(os.Stderr,
				"zero-memcp: 密钥已存在 %s\n"+
					"  覆盖会让所有已签发的凭证立即失效。真要换，加 -force。\n", path)
			return 1
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
			return 1
		}
	}

	if _, err := auth.LoadOrCreateSecret(path); err != nil {
		fmt.Fprintf(os.Stderr, "zero-memcp: %v\n", err)
		return 1
	}
	fmt.Printf("已生成 %s（%d 字节，0600）\n", path, auth.SecretSize)
	return 0
}
