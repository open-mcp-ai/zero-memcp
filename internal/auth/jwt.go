package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claims 是签发给 agent 的凭证载荷。
//
// 它只回答一个问题：**你是谁**。
//
// 不回答「你能干什么」——那是配置（用户归属、组、项目 mode）的事。
// 把权限塞进凭证里的后果是：改一次权限要重发所有凭证，而漏发一张
// 就变成一个「为什么它还是旧权限」的悬案。让凭证只带身份，
// 权限永远以配置文件为准，改配置即生效。
type Claims struct {
	// Sub 是使用者的名字，必须是配置里声明过的 [[user]]。
	Sub string `json:"sub"`
	Iss string `json:"iss"`
	Iat int64  `json:"iat"`

	// Exp 是过期时间戳，0 表示永不过期。
	Exp int64 `json:"exp"`

	// JTI 是这张凭证的唯一标识（16 位小写十六进制）。
	//
	// 没有它就吊销不了单张凭证：JWT 是无状态的，验签通过就放行，
	// 服务端没有任何办法说「这一张不算」。要能单独作废，载荷里必须
	// 先有一个能唯一叫出它的名字，吊销名单才有东西可写。
	JTI string `json:"jti,omitempty"`

	// Parent 是签发这张凭证的那张凭证的 jti。
	//
	// 有了它才能级联吊销：父凭证一废，整棵子树同时失效。没有它的话，
	// root 泄漏后被派生出的十张永久凭证，在你吊销 root 之后依然有效——
	// 那就等于没吊销。
	Parent string `json:"parent,omitempty"`

	// Cap 是能力上限，只能是 "rwx" 的子集，与 TOML 的权限取交集。
	//
	//	r  读内容
	//	w  写内容
	//	x  允许规则注入（/identity 会返回规则块）
	//
	// 空串表示不额外限制（等同 rwx），仍然受 TOML 约束。
	Cap string `json:"cap,omitempty"`

	// Mount 是挂载表。数组形式，因为**同一个仓库可以挂多次**：
	// 一次挂 chat 分支当根（可写），一次挂 zero 分支当只读参考。
	//
	//	[{"project":"daily","at":"/","branch":"chat","cap":"rw"},
	//	 {"project":"daily","at":"/main","branch":"zero","cap":"r"}]
	//
	// 不在表里的项目对这张凭证完全不可见。空表表示全命名空间（root）。
	Mount []MountPoint `json:"mount,omitempty"`

	// Roles 是命名预设：一个名字对应一组 (能力位, 挂载表)。
	//
	// 有 Roles 时，Cap/Mount 不再生效，改由「本次请求选中的角色」决定。
	// 选哪个角色由客户端在传输层给出（pi 的 /mem:role 命令写进 header），
	// **绝不能由 MCP 工具参数决定**——否则一次提示注入就是一次提权。
	Roles map[string]Role `json:"roles,omitempty"`

	// Default 是未指定角色时用哪个。空且 Roles 非空时一律拒绝：
	// 猜一个默认角色等于给注入留了一条不必提权也能越界的路。
	Default string `json:"default,omitempty"`

	// Ticket 为真表示这是一张兑换券，不是正式凭证。
	//
	// 券只用于换取正式 token，一次性、分钟级有效期。这样即使在对话里
	// 打印出来被记进上下文，也已经换过了，没有价值。
	Ticket bool `json:"tkt,omitempty"`
}

// MountPoint 是挂载表里的一项。
//
// 同时接受两种 JSON 写法：
//
//	"blackegg/me": "/"                     ← 简写
//	"blackegg/me": {"at":"/","cap":"rw"}   ← 完整
type MountPoint struct {
	// Project 是真实项目名，例如 "blackegg/daily"。
	Project string `json:"project"`
	// At 是它在这张凭证眼里的虚拟路径，"/" 表示成为根。
	At string `json:"at"`
	// Branch 是读写的分支。空表示用服务端默认分支。
	Branch string `json:"branch,omitempty"`
	// Cap 是这一个挂载点的能力上限，空表示继承凭证级 cap。
	Cap string `json:"cap,omitempty"`
}

// UnmarshalJSON 同时接受三种写法：
//
//	"blackegg/me"                                      ← 只给项目名，挂在根
//	{"project":"blackegg/me","at":"/","cap":"rw"}      ← 完整
//	"blackegg/me=/:rw"                                 ← 命令行简写
func (m *MountPoint) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*m = parseMountShorthand(s)
		return nil
	}
	type alias MountPoint
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = MountPoint(a)
	return nil
}

// parseMountShorthand 解析 "项目=路径:能力" 这种简写。
func parseMountShorthand(s string) MountPoint {
	pt := MountPoint{Project: strings.TrimSpace(s), At: "/"}
	if proj, rest, ok := strings.Cut(pt.Project, "="); ok {
		pt.Project = strings.TrimSpace(proj)
		at, capStr, _ := strings.Cut(rest, ":")
		if v := strings.TrimSpace(at); v != "" {
			pt.At = v
		}
		pt.Cap = strings.TrimSpace(capStr)
	}
	return pt
}

// Role 是一个命名预设：一组能力位 + 一张挂载表。
type Role struct {
	Cap   string       `json:"cap,omitempty"`
	Mount []MountPoint `json:"mount,omitempty"`
}

// TokenStore 持有签名密钥与验签能力。
type TokenStore struct {
	secret []byte
}

// NewTokenStore 初始化密钥库。传空密钥会随机生成一把——
// 那只适合测试；生产路径必须用 LoadOrCreateSecret 从磁盘读。
func NewTokenStore(secret []byte) *TokenStore {
	if len(secret) == 0 {
		secret = make([]byte, SecretSize)
		_, _ = rand.Read(secret)
	}
	return &TokenStore{secret: secret}
}

// Sign 签发一段 JWT 字符串。格式: header.payload.signature
func (ts *TokenStore) Sign(claims Claims) (string, error) {
	if claims.Iss == "" {
		claims.Iss = "zero-memcp"
	}
	if claims.Iat == 0 {
		claims.Iat = time.Now().Unix()
	}

	headerBytes, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	part1 := base64.RawURLEncoding.EncodeToString(headerBytes)
	part2 := base64.RawURLEncoding.EncodeToString(claimsBytes)
	signingInput := part1 + "." + part2

	mac := hmac.New(sha256.New, ts.secret)
	mac.Write([]byte(signingInput))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + signature, nil
}

// Verify 验证并解码 JWT 凭证。
func (ts *TokenStore) Verify(tokenString string) (*Claims, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, errors.New("非法 JWT 格式：应为三段式")
	}

	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("解码签名失败: %w", err)
	}

	mac := hmac.New(sha256.New, ts.secret)
	mac.Write([]byte(signingInput))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("凭证签名无效或已篡改")
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("解码 payload 失败: %w", err)
	}

	var claims Claims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, fmt.Errorf("解析 claims 失败: %w", err)
	}

	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, fmt.Errorf("凭证已于 %s 过期", time.Unix(claims.Exp, 0).Format(time.RFC3339))
	}
	if claims.Sub == "" {
		return nil, errors.New("凭证缺少 sub")
	}

	return &claims, nil
}
