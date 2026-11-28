package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 端到端演练真实威胁：agent 机器被攻破时会发生什么。
//
// 场景：攻击者拿到了 rwx 令牌（等价于完全控制 agent 进程），
// 他想抹掉一条对自己不利的记忆。他手里没有主人的私钥。
func TestCompromisedAgentCannotErase(t *testing.T) {
	// 主人在自己的设备上生成密钥对；服务端只拿到公钥
	masterPub, masterPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srv, dataDir := setupMountServer(t)
	kr := &masterKeyring{}
	ks := []masterKey{{Name: "master", Pub: masterPub, Root: true, Source: "test"}}
	kr.keys.Store(&ks)
	srv.masterKeys = kr
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	const path = "2026-11-19/evidence.md"
	if _, err := callMCPRaw(t, srv, tok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + path + " $BODY",
			"vars": map[string]string{"BODY": "对攻击者不利的记录"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero seed"); err != nil {
		t.Fatal(err)
	}

	// 攻击者第一步：直接 erase —— 拿到一张待签的挑战
	challenge, err := callMCP(t, srv, tok, "erase blackegg/me@zero:"+path)
	if err != nil {
		t.Fatalf("应当返回挑战而不是执行: %v", err)
	}
	if !strings.Contains(challenge, "requires an authorization ticket") {
		t.Fatalf("没有票据时必须索要授权，实际:\n%s", challenge)
	}

	// 攻击者第二步：自己的私钥签一张票（他有的是自己的密钥）
	attackerPub, attackerPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = attackerPub
	repo, _ := srv.projects.Get("blackegg/me")
	entry, _, _ := repo.Stat("zero", path)

	var nonce string
	for _, line := range strings.Split(challenge, "\n") {
		if i := strings.Index(line, "--nonce "); i >= 0 {
			rest := line[i+len("--nonce "):]
			nonce = strings.Fields(rest)[0]
			break
		}
	}
	if nonce == "" {
		t.Fatalf("挑战里没有 nonce:\n%s", challenge)
	}

	exp := time.Now().Add(time.Minute).Unix()
	forged := base64.StdEncoding.EncodeToString(
		ed25519.Sign(attackerPriv, []byte(eraseTicketPayload(entry.Hash, "blackegg/me", "zero", path, nonce, exp))))

	_, err = callMCP(t, srv, tok, "erase blackegg/me@zero:"+path+" --nonce "+nonce+" --sig "+forged)
	if err == nil {
		t.Fatal("攻击者用自己的私钥伪造的票必须被拒——否则这道门形同虚设")
	}
	if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("应明确指出验签失败，实际: %v", err)
	}
	// 关键：数据必须还在
	if _, found, _ := repo.Stat("zero", path); !found {
		t.Fatal("伪造被拒后数据必须完好，实际已消失")
	}
	t.Logf("攻破 agent 机器后仍无法抹除：%v", err)

	// 主人第三步：用自己的私钥签服务端给出的 payload —— 成功。
	// 从挑战里把那一整段提出来（人真实的操作就是这样复制的）。
	var challengePayload string
	lines := strings.Split(challenge, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "zero-memcp-erase-v1") {
			// 取后面 7 行
			end := i + 7
			if end > len(lines) {
				end = len(lines)
			}
			challengePayload = strings.Join(lines[i:end], "\n")
			break
		}
	}
	if challengePayload == "" {
		t.Fatalf("无法从挑战里抽取出待签串:\n%s", challenge)
	}
	good := base64.StdEncoding.EncodeToString(ed25519.Sign(masterPriv, []byte(challengePayload)))
	out, err := callMCP(t, srv, tok, "erase blackegg/me@zero:"+path+" --nonce "+nonce+" --sig "+good)
	if err != nil {
		t.Fatalf("主人的合法签名应当通过: %v", err)
	}
	if !strings.Contains(out, "erased") {
		t.Fatalf("应当抹除成功，实际:\n%s", out)
	}
	t.Logf("主人签名后成功抹除")
}

// 票据必须绑定内容哈希：签的是无害地址、却拿去抹别的，必须失败。
func TestEraseTicketBoundToBlob(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	srv, dataDir := setupMountServer(t)
	kr2 := &masterKeyring{}
	ks2 := []masterKey{{Name: "signer", Pub: pub, Root: true, Source: "test"}}
	kr2.keys.Store(&ks2)
	srv.masterKeys = kr2
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	for _, p := range []string{"2026-11-19/harmless.md", "2026-11-19/target.md"} {
		if _, err := callMCPRaw(t, srv, tok, map[string]any{
			"name": "mem",
			"arguments": map[string]any{
				"cmd":  "write blackegg/me@zero:" + p + " $BODY",
				"vars": map[string]string{"BODY": "内容 " + p},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 人签的是 harmless.md 那个 blob
	repo, _ := srv.projects.Get("blackegg/me")
	harmless, _, _ := repo.Stat("zero", "2026-11-19/harmless.md")
	nonce := "deadbeefdeadbeefdeadbeefdeadbeef"
	ticket := srv.issueEraseTicket("blackegg/me", "zero", "2026-11-19/harmless.md", harmless.Hash, nonce)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(ticket.Payload)))

	// 但拿它去抹 target.md
	_, err := callMCP(t, srv, tok, "erase blackegg/me@zero:2026-11-19/target.md --nonce "+nonce+" --sig "+sig)
	if err == nil {
		t.Fatal("签 A 抹 B 必须失败——票据绑定的是内容哈希，不是路径")
	}
	if _, found, _ := repo.Stat("zero", "2026-11-19/target.md"); !found {
		t.Fatal("target.md 不该被抹掉")
	}
	t.Logf("换目标被拦截: %v", err)
}
