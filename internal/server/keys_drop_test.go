package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 测试：通过 <data>/keys/ 目录放公钥文件，不用改动任何配置文件。
//
// 1. 放一个 phone.json 作为对等根钥（root: true）
// 2. 放一个 eraser.json 作为受限公钥（仅 caps: ["erase"]）
// 3. 验证：
//   - 两把钥匙自动生效、对等共存；
//   - eraser 只能批准 erase，拿它去签别的操作被拦；
//   - phone 拥有全权，可以批准任何操作。
func TestKeysDirectoryHotdrop(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	// 两对公私钥（模拟两台不同设备）
	phonePub, phonePriv, _ := ed25519.GenerateKey(rand.Reader)
	eraserPub, eraserPriv, _ := ed25519.GenerateKey(rand.Reader)

	keysDir := filepath.Join(dataDir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. 丢入 phone.toml（对等根钥，支持写注释！）
	phoneTOML := fmt.Sprintf(`# 主人的备用手机，拥有根权限
[key]
name = "phone-master"
public_key = %q
root = true
`, base64.StdEncoding.EncodeToString(phonePub))
	if err := os.WriteFile(filepath.Join(keysDir, "phone.toml"), []byte(phoneTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. 丢入 eraser.toml（受限权限位：仅 erase）
	eraserTOML := fmt.Sprintf(`# 销毁专用钥匙：只能签署 erase 挑战，不能改规则
[key]
name = "eraser-device"
public_key = %q
caps = ["erase"]
`, base64.StdEncoding.EncodeToString(eraserPub))
	if err := os.WriteFile(filepath.Join(keysDir, "eraser.toml"), []byte(eraserTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	// 重新载入钥匙（启动或 reload 时发生）
	srv.loadMasterKeys()

	if srv.masterKeys == nil || srv.masterKeys.count() != 2 {
		t.Fatalf("keys/ 目录下 2 把钥匙应当全部生效，实际 loaded: %d", srv.masterKeys.count())
	}
	t.Logf("生效钥匙清单: %s", strings.Join(srv.masterKeys.describeLines(), " | "))

	// 写测试数据
	rwxTok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})
	const path = "2026-11-19/test-drop.md"
	_, _ = callMCPRaw(t, srv, rwxTok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + path + " $BODY",
			"vars": map[string]string{"BODY": "待销毁数据"},
		},
	})
	_, _ = callMCP(t, srv, rwxTok, "commit blackegg/me@zero init")

	// 触发挑战
	res, _ := callMCP(t, srv, rwxTok, "erase blackegg/me@zero:"+path)
	payload := betweenMarkers(res)
	if payload == "" {
		t.Fatalf("未拿到挑战 payload:\n%s", res)
	}
	nonce := ""
	for _, l := range strings.Split(res, "\n") {
		if strings.Contains(l, "--nonce ") {
			nonce = strings.Fields(l)[len(strings.Fields(l))-3]
			break
		}
	}

	// 3. 用受限的 eraser 私钥签署 erase 挑战：应当成功！
	sigEraser := base64.StdEncoding.EncodeToString(ed25519.Sign(eraserPriv, []byte(payload)))
	eraseCmd := "erase blackegg/me@zero:" + path + " --nonce " + nonce + " --sig " + sigEraser
	out, err := callMCP(t, srv, rwxTok, eraseCmd)
	if err != nil {
		t.Fatalf("受限钥匙签所属权限范围内的 erase 应当成功，实际报错: %v", err)
	}
	if !strings.Contains(out, "erased") {
		t.Fatalf("erase 输出不符: %s", out)
	}
	t.Logf("eraser-device 成功授权抹除: %s", out)

	// 4. 再次写入一条，用 replace（需要 replace 权限位）测试越权拦截
	const path2 = "2026-11-19/replace-test.md"
	_, _ = callMCPRaw(t, srv, rwxTok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero:" + path2 + " $BODY",
			"vars": map[string]string{"BODY": "待替换数据"},
		},
	})
	_, _ = callMCP(t, srv, rwxTok, "commit blackegg/me@zero seed2")

	res2, _ := callMCPRaw(t, srv, rwxTok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  "replace blackegg/me@zero:" + path2 + " $BODY",
			"vars": map[string]string{"BODY": "[已抹除]"},
		},
	})
	payload2 := betweenMarkers(res2)
	nonce2 := ""
	for _, l := range strings.Split(res2, "\n") {
		if strings.Contains(l, "--nonce ") {
			nonce2 = strings.Fields(l)[len(strings.Fields(l))-3]
			break
		}
	}

	// 尝试用仅有 erase 权限的 eraserPriv 去批准 replace：必须被拦！
	sigIllegal := base64.StdEncoding.EncodeToString(ed25519.Sign(eraserPriv, []byte(payload2)))
	illegalCmd := "replace blackegg/me@zero:" + path2 + " --nonce " + nonce2 + " --sig " + sigIllegal + " $BODY"
	_, err = callMCPRaw(t, srv, rwxTok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  illegalCmd,
			"vars": map[string]string{"BODY": "[已抹除]"},
		},
	})
	if err == nil {
		t.Fatal("只有 erase 权限的钥匙去批准 replace 操作必须被拦截！")
	}
	if !strings.Contains(err.Error(), "caps do not include it") {
		t.Fatalf("报错应指出权限位不足，实际报错: %v", err)
	}
	t.Logf("越权拦截成功: %v", err)

	// 换用拥有 root 权限的 phonePriv 签署同一挑战：成功！
	sigRoot := base64.StdEncoding.EncodeToString(ed25519.Sign(phonePriv, []byte(payload2)))
	legalCmd := "replace blackegg/me@zero:" + path2 + " --nonce " + nonce2 + " --sig " + sigRoot + " $BODY"
	repOut, err := callMCPRaw(t, srv, rwxTok, map[string]any{
		"name": "mem",
		"arguments": map[string]any{
			"cmd":  legalCmd,
			"vars": map[string]string{"BODY": "[已抹除]"},
		},
	})
	if err != nil {
		t.Fatalf("拥有 root 的对等根钥应当能批准 replace: %v", err)
	}
	if !strings.Contains(repOut, "replaced") {
		t.Fatalf("replace 输出不符: %s", repOut)
	}
	t.Logf("phone-master 对等根钥成功授权 replace: %s", repOut)
}
