// zero-memcp 是记忆服务。
//
// 记忆只从 MCP 进去。CLI 不是数据入口：
// 一旦终端也能读写记忆，那条路径就不经任何凭证，Caller 变成写死的 root，
// 审计里的 source 也不再是真实身份。权限管理由 WebUI 承载；CLI 只负责起服务、
// 打印版本与帮助。
package main

import (
	"fmt"
	"os"
)

// version 由构建时注入：-ldflags "-X main.version=...".
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version", "-V", "--version":
			fmt.Printf("zero-memcp %s\n", version)
			return
		case "help", "-h", "--help":
			usage(os.Stdout)
			return
		case "serve":
			args = args[1:]
		}
	}
	os.Exit(runServe(args))
}

func usage(w *os.File) {
	fmt.Fprint(w, `zero-memcp - a Git-backed memory store for agents (MCP server)

Memory only enters through MCP. The CLI neither reads nor writes memory,
and it no longer manages credentials or permissions - that lives in the WebUI.

Start the server:
  zero-memcp serve -data DIR [-config FILE] [-listen ADDR] [-endpoint NAME]
                     [-branch NAME] [-weights FILE]
  zero-memcp -data DIR ...   (legacy form, identical to serve)

  On first start it generates the signing key (system/jwt.key) and an initial
  WebUI password. The password is printed once to stderr and must be changed
  after the first login. Set ZERO_MEMCP_UI_PASS before startup to choose your
  own initial password instead of a generated one.

Help and version:
  zero-memcp help
  zero-memcp version

Permission model:
  An MCP credential carries a capability ceiling (cap) and a mount table
  (mount); the config layer narrows it further. A credential can only ever
  shrink access, never widen it.
  Being root is decided solely by [[user]] root = true in the config - not by
  a name, a capability bit, or a wildcard.
  A credential whose write access must be activated on demand can require WebUI
  grants at mint time; grants are scoped by JTI, project, branch and expiry.

WebUI:
  The admin surface (/, /ui/*) always exists - it is the only way to change the
  initial password, mint credentials, or grant gated capability bits, and in a
  container or behind a wrapper nobody can run a command. Which port carries it
  is configurable: with [[listen]] blocks, set webui = true on one of them to
  isolate the surface (other ports then serve no admin route at all, and reply
  404); with none set it lands on the first port. The startup log prints the URL.
  The WebUI authenticates with its own password-backed session; MCP uses Bearer
  credentials. The two never accept each other's credentials.
`)
}
