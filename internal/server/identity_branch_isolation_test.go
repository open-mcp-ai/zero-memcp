package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// callIdentity 取 /identity 的原始 JSON 响应。
func callIdentity(t *testing.T, srv *Server, tok string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/identity", nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	srv.Mux().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("identity failed (%d): %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// When a project has multiple branches with rules (e.g. 'zero' for me,
// 'chat' for a QQ bot), identity must inject from ONE branch per project -
// the credential's default or active branch.
//
// Injecting from every branch concatenated conflicting identities:
// "I am the QQ bot" alongside "I am Jane", plus duplicated the rules of
// whichever branch had multiple copies. The model saw both personas at once.
func TestIdentityPicksSingleBranchPerProject(t *testing.T) {
	srv, dataDir := setupMountServer(t)

	// Seed two distinct identity branches into blackegg/me.
	root := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "root", Cap: "rwx"})

	// Branch 'zero': Jane's rules
	if _, err := callMCPWithVars(t, srv, root, "write blackegg/me@zero/SKILL.md -- $B",
		map[string]string{"B": "---\ninject: every_turn\n---\n# Jane persona"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, root, "commit blackegg/me@zero -m zero"); err != nil {
		t.Fatal(err)
	}

	// Branch 'chat': QQ bot rules
	if _, err := callMCP(t, srv, root, "branch blackegg/me chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPWithVars(t, srv, root, "write blackegg/me@chat/SKILL.md -- $B",
		map[string]string{"B": "---\ninject: every_turn\n---\n# QQ bot persona"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, root, "commit blackegg/me@chat -m chat"); err != nil {
		t.Fatal(err)
	}

	// Credential mounted to the default branch (or wildcard).
	callerTok := mintRestricted(t, dataDir, auth.Claims{
		Sub:  "zero",
		Name: "caller",
		Cap:  "rwx",
		Mount: []auth.MountPoint{
			{Project: "blackegg/me", At: "/", Branch: "zero"},
		},
	})

	body := callIdentity(t, srv, callerTok)
	if !strings.Contains(body, "Jane persona") {
		t.Fatalf("caller mounted to zero should see Jane's persona, got:\n%s", body)
	}
	if strings.Contains(body, "QQ bot persona") {
		t.Fatalf("rules from branch 'chat' must NOT leak into the zero identity:\n%s", body)
	}

	// And conversely, a credential pinned to the chat branch sees only the bot.
	chatTok := mintRestricted(t, dataDir, auth.Claims{
		Sub:  "zero",
		Name: "chat",
		Cap:  "rwx",
		Mount: []auth.MountPoint{
			{Project: "blackegg/me", At: "/", Branch: "chat"},
		},
	})
	chatBody := callIdentity(t, srv, chatTok)
	if !strings.Contains(chatBody, "QQ bot persona") {
		t.Fatalf("chat caller should see the bot persona:\n%s", chatBody)
	}
	if strings.Contains(chatBody, "Jane persona") {
		t.Fatalf("Jane's persona must not leak into the chat branch:\n%s", chatBody)
	}
}

// When a credential has an unrestricted or wildcard mount, it must still
// pick the project's single default branch instead of concatenating every branch.
func TestIdentityWildcardMountDoesNotConcatenateBranches(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	root := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "root", Cap: "rwx"})

	if _, err := callMCPWithVars(t, srv, root, "write blackegg/me@zero/SKILL.md -- $B",
		map[string]string{"B": "---\ninject: every_turn\n---\n# Jane"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, root, "commit blackegg/me@zero -m zero"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, root, "branch blackegg/me chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPWithVars(t, srv, root, "write blackegg/me@chat/SKILL.md -- $B",
		map[string]string{"B": "---\ninject: every_turn\n---\n# Bot"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, root, "commit blackegg/me@chat -m chat"); err != nil {
		t.Fatal(err)
	}

	// Unrestricted root caller (no mount table = full visibility).
	body := callIdentity(t, srv, root)
	janeCount := strings.Count(body, "Jane")
	botCount := strings.Count(body, "Bot")

	if janeCount != 1 {
		t.Fatalf("Jane must be injected exactly once, got %d", janeCount)
	}
	if botCount != 0 {
		t.Fatalf("unrestricted caller defaults to the primary branch, must not concatenate 'chat':\n%s", body)
	}
}
