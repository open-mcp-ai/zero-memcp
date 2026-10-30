package server

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 级联吊销：父凭证一旦吊销，沿父链派生出的一切子凭证全部失效。
func TestCascadeRevocation(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	secret, err := auth.LoadOrCreateSecret(filepath.Join(dataDir, "system", "jwt.key"))
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewTokenStore(secret)
	rev := auth.NewRevocations(filepath.Join(dataDir, "system", "revoked"))

	// 1. 父凭证 P
	pJTI, _ := auth.NewJTI()
	_, err = store.Sign(auth.Claims{
		Sub: "zero",
		Exp: time.Now().Add(time.Hour).Unix(),
		JTI: pJTI,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. 子凭证 C，记录 parent = pJTI
	cJTI, _ := auth.NewJTI()
	if err := rev.RecordParent(cJTI, pJTI); err != nil {
		t.Fatal(err)
	}
	cTok, err := store.Sign(auth.Claims{
		Sub:    "zero",
		Exp:    time.Now().Add(time.Hour).Unix(),
		JTI:    cJTI,
		Parent: pJTI,
		Mount:  []auth.MountPoint{{Project: "blackegg/me", At: "/"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. 吊销前：子凭证正常可用
	out, err := callMCP(t, srv, cTok, "read 01-shape.md")
	if err != nil || !strings.Contains(out, "形状") {
		t.Fatalf("吊销前子凭证应正常可用: %v, out=%s", err, out)
	}

	// 4. 只吊销父凭证 P（C 本身没进 revoked/ 目录）
	if err := rev.Revoke(pJTI); err != nil {
		t.Fatal(err)
	}

	// 5. 子凭证 C 必须立即被拒绝
	_, err = callMCP(t, srv, cTok, "read 01-shape.md")
	if err == nil || !strings.Contains(err.Error(), "吊销") {
		t.Fatalf("父凭证已吊销，子凭证应被级联拒绝，实际: %v", err)
	}
}
