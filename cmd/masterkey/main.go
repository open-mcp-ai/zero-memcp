// masterkey 是主人（人）侧的授权工具：生成密钥对、签署销毁密信。
//
// # 为什么它是一个独立的小工具
//
// 私钥**绝不能**进 zero-memcp 的数据目录，也不能进 agent 的进程。
// 威胁模型是「agent 那台机器不一定安全」——如果私钥和公钥放在同一台
// 机器上，这道门就白设了。
//
// 所以这个工具是给你在自己电脑上、离线或本地运行的：
//
//	masterkey gen -out ~/.zero-master         生成密钥对（私钥留在本机）
//	masterkey sign -key ~/.zero-master.key \  签署一张服务端给的挑战
//	    -payload 'zero-memcp-erase-v1
//	4b825dc...
//	blackegg/me
//	zero
//	2026-11-19/leak.md
//	7e2742...
//	1795150779'
//
// 然后把输出的 base64 签名填进 erase 命令的 --sig。
//
// 服务端只持有公钥：它无法伪造密信，也无法反推出私钥。即使那台机器
// 被完全攻破，攻击者能做的只是**拒绝**你的销毁请求，而不是**伪造**一个。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gen", "keygen":
		runGen(os.Args[2:])
	case "sign":
		runSign(os.Args[2:])
	case "pub":
		runPub(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `masterkey - authorization key for zero-memcp destructive operations

The private key NEVER goes to the server. The server holds only the public
key, so a compromised agent machine can refuse your request but can never
forge one.

  masterkey gen  [-out DIR]     generate a keypair (private stays on this machine)
  masterkey pub  [-key FILE]    print the public key to paste into the config
  masterkey sign -key FILE -payload STRING
                                sign a challenge payload from the server

Typical flow:
  1. masterkey gen -out ~/.zero-master
  2. put the printed public key into zero-memcp.toml:
         [erase]
         public_key = "<base64>"
  3. run the destructive command; the server replies with a payload to sign
  4. masterkey sign -key ~/.zero-master/private.key -payload '<that payload>'
  5. re-run the command with --nonce <n> --expires <t> --sig <signature>
`)
}

func runGen(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	out := fs.String("out", ".", "directory for private.key and public.txt")
	_ = fs.Parse(args)

	if err := os.MkdirAll(*out, 0o700); err != nil {
		die("create %s: %v", *out, err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		die("generate key: %v", err)
	}

	// 私钥用 PKCS#8 + PEM 存，权限 0600。
	// 用标准格式而不是裸字节：出了事能被 openssl 读出来，
	// 而「只有我这个程序能读」不是优点。
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		die("marshal private key: %v", err)
	}
	privPath := filepath.Join(*out, "private.key")
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(block), 0o600); err != nil {
		die("write %s: %v", privPath, err)
	}

	pubB64 := base64.StdEncoding.EncodeToString(pub)
	pubPath := filepath.Join(*out, "public.txt")
	if err := os.WriteFile(pubPath, []byte(pubB64+"\n"), 0o644); err != nil {
		die("write %s: %v", pubPath, err)
	}

	fmt.Printf("private key: %s   (mode 0600 - keep it on THIS machine)\n", privPath)
	fmt.Printf("public key:  %s\n\n", pubPath)
	fmt.Printf("add this to zero-memcp.toml:\n\n  [erase]\n  public_key = %q\n\n", pubB64)
	fmt.Println("the server can verify your signatures but can never create one.")
}

func runPub(args []string) {
	fs := flag.NewFlagSet("pub", flag.ExitOnError)
	keyPath := fs.String("key", "private.key", "path to the private key")
	_ = fs.Parse(args)

	priv := loadPrivate(*keyPath)
	pub := priv.Public().(ed25519.PublicKey)
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
}

func runSign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "private.key", "path to the private key")
	payload := fs.String("payload", "", "the exact challenge payload printed by the server")
	payloadFile := fs.String("payload-file", "", "read the payload from a file instead")
	_ = fs.Parse(args)

	msg := *payload
	if *payloadFile != "" {
		raw, err := os.ReadFile(*payloadFile)
		if err != nil {
			die("read payload file: %v", err)
		}
		msg = string(raw)
	}
	if msg == "" {
		die("sign needs -payload or -payload-file (copy it exactly from the server's reply)")
	}
	// 容忍粘贴时附带的一个尾部换行。
	//
	// 判据是「意图」：真正的待签串以时间戳数字结尾，不可能以换行结尾。
	// 而从终端、编辑器、命令替换里粘过来时，几乎必然多一个 \n。
	// 为此让人对着「签名不通过」猜半天，是把工具的笨拙转嫁给使用者。
	msg = strings.TrimRight(msg, "\r\n")

	priv := loadPrivate(*keyPath)
	sig := ed25519.Sign(priv, []byte(msg))
	fmt.Println(base64.StdEncoding.EncodeToString(sig))
}

func loadPrivate(path string) ed25519.PrivateKey {
	raw, err := os.ReadFile(path)
	if err != nil {
		die("read %s: %v", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		die("%s is not a PEM file", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		die("parse %s: %v", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		die("%s holds a %T, not an ed25519 key", path, key)
	}
	return priv
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "masterkey: "+format+"\n", a...)
	os.Exit(1)
}
