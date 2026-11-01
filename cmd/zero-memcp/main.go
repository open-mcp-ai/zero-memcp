// zero-memcp 是记忆服务。
//
// 它读若干仓库（裸 Git 仓库或普通目录），按每个仓库里 SKILL.md 的声明
// 决定注入、索引与写入，然后在 127.0.0.1:1729 上以 Streamable HTTP MCP
// 对外服务。
//
// 命令行有两种形态：
//
//	zero-memcp [启动参数]              启动服务
//	zero-memcp token mint|list|revoke  签发与管理凭证
//	zero-memcp keygen                  只生成密钥后退出
//	zero-memcp gc                      打包仓库对象
package main

import (
	"fmt"
	"os"
)

func main() {
	// 不带子命令时就是启动服务，保持对老调用方式的兼容——
	// 容器里的启动命令不能因为我们加了一个子命令就失效。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "token":
			os.Exit(runToken(os.Args[2:]))
		case "keygen":
			os.Exit(runKeygen(os.Args[2:]))
		case "gc":
			os.Exit(runGC(os.Args[2:]))
		case "help", "-h", "--help":
			usage(os.Stdout)
			return
		case "version", "-V", "--version":
			fmt.Println("zero-memcp")
			return
		}
	}
	os.Exit(runServe(os.Args[1:]))
}

func usage(w *os.File) {
	fmt.Fprint(w, `zero-memcp — 记忆服务

启动：
  zero-memcp -data DIR [-config FILE] [-listen ADDR] [-endpoint NAME]
  zero-memcp -data DIR -config FILE [-branch NAME]    （指定默认分支）
  zero-memcp -data DIR -dump                          （只打印扫描结果后退出）

凭证：
  zero-memcp token mint -data DIR -user NAME [-name CRED] [-exp DUR]
                        [-cap rwx] [-mount 项目=路径:能力,...] [-parent JTI]
                        [-ticket]
  zero-memcp token list -data DIR
  zero-memcp token revoke -data DIR (-jti JTI | -name CRED)
  zero-memcp token unrevoke -data DIR -jti JTI

维护：
  zero-memcp keygen -data DIR [-force]   生成签名密钥（已存在则报错，不覆盖）
  zero-memcp gc -data DIR [-project NS] [-dry-run]

数据布局：
  DIR/system/jwt.key                 签名密钥（0600，泄露等于所有凭证可伪造）
  DIR/system/tokens/<name>.json      签发记录（审计用，不参与验签）
  DIR/system/revoked/<jti>           吊销名单（一个 jti 一个空文件）
  DIR/repos/<ns>.git                 裸仓库；每条分支是一条独立版本线

权限：命名空间 + 能力字母，签发时定死，只能收窄。
  me:r      只读（recall / read / grep / ls）
  archive:rw  可写（put / commit）
  me:rx     x 允许作为规则内容被读取（比只读多一项能力）
  *:rwx     全命名空间，即管理员凭证

  管理员凭证不是另一套机制，就是通配符：-root 等价于 -allow '*:rwx'。
`)
}
