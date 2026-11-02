package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// put auto-creates the repository for a declared project; write must behave the
// same way. Otherwise the same target answers "ok" for put and "repository does
// not exist" for write - and write is the verb that writes a file at a path.
func TestWriteCreatesRepositoryLikePut(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	out, err := callMCP(t, srv, tok, "write blackegg/me@chat/first.md -- hello")
	if err != nil {
		t.Fatalf("write on a declared project with no repository yet must succeed: %v", err)
	}
	if !strings.Contains(out, "written to blackegg/me@chat/first.md") {
		t.Fatalf("unexpected write result: %s", out)
	}
	read, err := callMCP(t, srv, tok, "read blackegg/me@chat/first.md")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(read, "hello") {
		t.Fatalf("content not stored: %s", read)
	}
}

// A repository open failure must not tell the caller where the data lives.
//
// gitstore reports absolute paths for operators, but an MCP caller only ever
// sees virtual addresses. Leaking "/var/lib/.../repos/x.git" both breaks the
// address model and reveals the host layout to an unprivileged credential.
func TestRepoOpenErrorHidesServerPath(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	_, err := srv.projects.Get("missing-project")
	if err == nil {
		t.Fatal("expected an error for a project with no repository")
	}
	msg := err.Error()
	if strings.Contains(msg, dataDir) {
		t.Fatalf("error leaked the data directory: %s", msg)
	}
	if strings.Contains(msg, "/repos/") || strings.Contains(msg, ".git") {
		t.Fatalf("error leaked a repository path: %s", msg)
	}
	if !strings.Contains(msg, "missing-project") {
		t.Fatalf("error should still name the project: %s", msg)
	}
}
