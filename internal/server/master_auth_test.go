package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 测试：主人的私钥签名（密信）机制。
//
// 威胁模型：Agent 所在的机器不一定安全，上面只能放公钥；
// 真正的人（主人）在自己的电脑上持有私钥。
// 即使 Agent 的 token 泄露，攻击者没有主人的私钥，也绝对无法执行世界树改写！
func TestMasterKeyEraseAuthorization(t *testing.T) {
	// 1. 人在自己的离线设备上生成一对公私钥
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	srv, dataDir := setupMountServer(t)

	// 将公钥配置给服务端（服务端只有公钥，没有私钥！）
	kr := &masterKeyring{}
	ks := []masterKey{{Name: "master", Pub: pub, Root: true, Source: "test"}}
	kr.keys.Store(&ks)
	srv.masterKeys = kr

	rwxTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 先写一条敏感记忆
	const targetPath = "2026-11-19/master-secret.md"
	if _, err := callMCPRaw(t, srv, rwxTok, map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + targetPath + " $BODY",
			"vars": map[string]string{"BODY": "绝密资料，只有主人能决定销毁"},
		},
	}); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if _, err := callMCP(t, srv, rwxTok, "commit blackegg/me@zero init"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 阶段一：攻击者或者 Agent 自身直接尝试调用 erase
	// 期望：因为没有主人的签名密信，操作必须被拦截，并返回要求签名的挑战文本！
	challengeRes, err := callMCP(t, srv, rwxTok, "erase blackegg/me@zero:"+targetPath)
	if err != nil {
		t.Fatalf("第一阶段应返回挑战文本而不是直接 crash: %v", err)
	}
	if !strings.Contains(challengeRes, "BEGIN PAYLOAD") || !strings.Contains(challengeRes, "--nonce") {
		t.Fatalf("第一阶段必须出声索要主人密信，实际输出为:\n%s", challengeRes)
	}
	t.Logf("第一阶段挑战文本成功拦截，提示信息:\n%s", challengeRes)

	// 提取出返回的 nonce 和 payload
	repo, _ := srv.projects.Get("blackegg/me")
	entry, found, err := repo.Stat("zero", targetPath)
	if err != nil || !found {
		t.Fatal("未找到刚刚写的 blob")
	}

	// 从挑战文本中提取 nonce
	var nonce string
	for _, line := range strings.Split(challengeRes, "\n") {
		if strings.Contains(line, "--nonce ") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "--nonce" && i+1 < len(parts) {
					nonce = parts[i+1]
					break
				}
			}
		}
	}
	if nonce == "" {
		t.Fatalf("无法从挑战中解析 nonce: %s", challengeRes)
	}

	// 阶段二：黑客尝试用伪造的私钥签名
	_, fakePriv, _ := ed25519.GenerateKey(nil)
	fakePayload := eraseTicketPayload(entry.Hash, "blackegg/me", "zero", targetPath, nonce, time.Now().Add(1*time.Minute).Unix())
	fakeSig := base64.StdEncoding.EncodeToString(ed25519.Sign(fakePriv, []byte(fakePayload)))

	fakeCmd := fmt.Sprintf("erase blackegg/me@zero:%s --nonce %s --sig %s",
		targetPath, nonce, fakeSig)
	_, err = callMCP(t, srv, rwxTok, fakeCmd)
	if err == nil {
		t.Fatal("使用伪造的私钥签名必须被服务端拒绝！")
	}
	if !strings.Contains(err.Error(), "does not verify against any configured master key") {
		t.Fatalf("报错应明确指出签名验证失败，实际报错: %v", err)
	}
	t.Logf("黑客伪造签名成功被拦截: %v", err)

	// 阶段三：真正的主人拿出自己的私钥签名，发出密信！
	var realPayload string
	lines := strings.Split(challengeRes, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "zero-memcp-erase-v1") {
			end := i + 7
			if end > len(lines) {
				end = len(lines)
			}
			realPayload = strings.Join(lines[i:end], "\n")
			break
		}
	}
	if realPayload == "" {
		t.Fatalf("无法从挑战中抽取 payload: %s", challengeRes)
	}
	realSig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(realPayload)))

	masterCmd := fmt.Sprintf("erase blackegg/me@zero:%s --nonce %s --sig %s",
		targetPath, nonce, realSig)
	successRes, err := callMCP(t, srv, rwxTok, masterCmd)
	if err != nil {
		t.Fatalf("主人提供合法密信后应当成功执行世界树抹除，实际报错: %v", err)
	}
	if !strings.Contains(successRes, "erased blackegg/me@zero") {
		t.Fatalf("应成功销毁，实际输出:\n%s", successRes)
	}
	t.Logf("主人下达密信后成功执行抹除:\n%s", successRes)

	// 阶段四：防重放攻击！黑客偷到了这封签过名的密信，想再次使用它去抹除
	// 期望：单次使用失效，立即被拒绝！
	_, err = callMCP(t, srv, rwxTok, masterCmd)
	if err == nil {
		t.Fatal("同一张票据（密信）二次使用必须被防重放拦截！")
	}
	if !strings.Contains(err.Error(), "already used") {
		t.Fatalf("应指出票据已使用过，实际报错: %v", err)
	}
	t.Logf("防重放成功拦截密信二次使用: %v", err)
}
