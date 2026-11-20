package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// 销毁授权票据（erase ticket）。
//
// # 为什么需要
//
// 世界树改写是唯一能物理销毁数据的操作。它面对的威胁不是「坏人读懂密文」，
// 而是「有人拿到了 agent 的令牌，然后下一条命令把记忆抹掉」。加密对这个
// 威胁完全无用——偷到令牌的人不需要读任何密文，直接下令即可。
//
// 唯一有效的手段是**数字签名**：私钥在人的设备上，服务端只持有公钥。
// agent 进程里没有私钥，于是令牌被偷也造不出有效票据。
//
// # 必须签什么
//
// 最容易漏的一环：只签路径等于没签。签一个无害地址、改参数去抹别的，
// 就绕过了。所以票据覆盖的是**内容哈希本身**：
//
//	zero-memcp-erase-v1\n<blob_sha>\n<project>\n<branch>\n<path>\n<nonce>\n<expires_unix>
//
// 服务端验签后再比对「你签的那个 blob 是不是当前这条路径指向的 blob」，
// 不一致就拒绝——否则「先签后换」是一条稳定的绕过路径。
//
// # 不做的两件事
//
//  1. 不做「服务端用公钥加密发给客户端」：那是保密方向，谁都能发明文，
//     服务端依然不知道谁发的；而且 nonce 会进日志，泄露面变大。
//  2. 不把私钥存进 data 目录：那和 token 一样会被偷。私钥只能在人手上。

// eraseTicketTTL 是票据有效期。
//
// 短是刻意的：票据的唯一用途是「证明此刻有人在场并同意」，它不是长期凭证。
// 两分钟足够人看一遍影响面再签，又短到没有多少重放空间。
const eraseTicketTTL = 2 * time.Minute

// 注意：单钥模型（eraseTicketSigner）已由 masterkeys.go 的密钥环取代。
// 一把钥只能表达「能做/不能做」，而真实场景需要「谁能做什么」——
// 归档钥只能抹除、运维钥只能改规则。权限位是那件事的最小表达。

// eraseTicket 是一张已签发、待签名的挑战。
type eraseTicket struct {
	Nonce     string `json:"nonce"`
	BlobSHA   string `json:"blob_sha"`
	Payload   string `json:"payload"` // 需要被签名的规范化串
	ExpiresAt int64  `json:"expires_at"`
}

// issueEraseTicket 为一次销毁签发挑战。
func (s *Server) issueEraseTicket(project, branch, path, blobSHA, nonce string) eraseTicket {
	expires := time.Now().Add(eraseTicketTTL).Unix()
	payload := eraseTicketPayload(blobSHA, project, branch, path, nonce, expires)
	if s.masterKeys != nil {
		s.masterKeys.issued.Store(nonce, expires)
	}
	return eraseTicket{Nonce: nonce, BlobSHA: blobSHA, Payload: payload, ExpiresAt: expires}
}

// eraseTicketPayload 是参与签名的规范化串。
//
// 字段顺序与分隔符写死在这里，任何改动都等于换协议——改它就会让所有
// 老客户端签出的票据失效，所以它必须显式带版本前缀。
func eraseTicketPayload(blobSHA, project, branch, path, nonce string, expires int64) string {
	return fmt.Sprintf("zero-memcp-erase-v1\n%s\n%s\n%s\n%s\n%s\n%d",
		blobSHA, project, branch, path, nonce, expires)
}

// verifyEraseTicket 校验票据。
//
// 校验顺序是刻意的：先查 nonce 是否用过（防重放，最便宜），再看是否过期，
// 最后才验签（最贵）。任何一步不过都当场拒绝并说明原因——「静默失败」
// 在销毁操作上是最坏的结果：人以为抹掉了，其实没抹。

// ticketFlags 是从命令行里抽出的票据参数。
type ticketFlags struct {
	nonce   string
	sig     string
	expires int64
}

// extractTicketFlags 摘下 --nonce / --sig / --expires 并返回剩余文本。
func extractTicketFlags(rest string) (ticketFlags, string, error) {
	var tk ticketFlags
	fields := strings.Fields(rest)
	keep := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		take := func(name string) (string, error) {
			if i+1 >= len(fields) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return fields[i], nil
		}
		var err error
		switch f {
		case "--nonce":
			tk.nonce, err = take("--nonce")
		case "--sig":
			tk.sig, err = take("--sig")
		case "--expires":
			var v string
			v, err = take("--expires")
			if err == nil {
				_, _ = fmt.Sscanf(v, "%d", &tk.expires)
			}
		default:
			keep = append(keep, f)
		}
		if err != nil {
			return tk, "", err
		}
	}
	return tk, strings.Join(keep, " "), nil
}

// newTicketNonce 生成一个随机 nonce（128 bit）。
func newTicketNonce() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
