// mintcred 为本机签一张命名凭证。
//
// 为什么需要它：凭证的挂载表把 map 里的「项目名」写死在 JWT 载荷里
// （mount[].project）。项目改名（zero → dev/zero）之后，旧凭证的
// 投影全部落空，表现是「read/put 都报 outside this credential's
// visible scope」——不是权限配错了，是名字对不上了。
//
// 用法：
//   mintcred -sub weft -name weft -cap rwx \
//     -mount dev/weft@main=rwx -mount dev/weft@main.record=rwx \
//     -mount dev/zero@main.record=r
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

type mountList []auth.MountPoint

func (m *mountList) String() string { return "" }
func (m *mountList) Set(v string) error {
	// 形如 project@branch=cap
	capStr := ""
	if i := strings.LastIndex(v, "="); i >= 0 {
		capStr = v[i+1:]
		v = v[:i]
	}
	at := strings.Index(v, "@")
	if at < 0 {
		return fmt.Errorf("mount must be project@branch[=cap]: %q", v)
	}
	*m = append(*m, auth.MountPoint{
		Project: v[:at],
		At:      v[:at],
		Branch:  v[at+1:],
		Cap:     capStr,
	})
	return nil
}

func main() {
	sub := flag.String("sub", "", "user name declared in [[user]]")
	name := flag.String("name", "", "credential name (default = sub)")
	capStr := flag.String("cap", "rwx", "credential capability ceiling")
	dataDir := flag.String("data", os.ExpandEnv("$HOME/.zero-memcp/data"), "data dir")
	var mounts mountList
	flag.Var(&mounts, "mount", "project@branch[=cap]; repeatable")
	flag.Parse()

	if *sub == "" {
		fmt.Fprintln(os.Stderr, "mintcred: -sub is required")
		os.Exit(1)
	}
	credName := *name
	if credName == "" {
		credName = *sub
	}

	key, err := os.ReadFile(filepath.Join(*dataDir, "system", "jwt.key"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mintcred: %v\n", err)
		os.Exit(1)
	}
	ts := auth.NewTokenStore(key)

	rb := make([]byte, 8)
	if _, err := rand.Read(rb); err != nil {
		fmt.Fprintf(os.Stderr, "mintcred: %v\n", err)
		os.Exit(1)
	}
	jti := hex.EncodeToString(rb)

	claims := auth.Claims{
		Sub: *sub, Name: credName, Iat: time.Now().Unix(), Exp: 0,
		JTI: jti, Cap: *capStr, Mount: mounts,
	}
	tok, err := ts.Sign(claims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mintcred: %v\n", err)
		os.Exit(1)
	}

	rec := auth.TokenRecord{
		Name: credName, JTI: jti, Sub: *sub, Exp: 0,
		Created: time.Now().Unix(), Cap: *capStr, Mount: mounts,
		CapRecorded: true, MountRecorded: true,
	}
	raw, _ := json.MarshalIndent(rec, "", "  ")
	tokDir := filepath.Join(*dataDir, "system", "tokens")
	if err := os.MkdirAll(tokDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mintcred: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(tokDir, credName+".json"), raw, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "mintcred: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
