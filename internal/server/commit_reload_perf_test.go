package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// Commit should be fast (milliseconds, not minutes) and must not rebuild
// the whole in-memory index from scratch.
//
// When tens of thousands of documents are already indexed, Reload() reads
// every single file on disk and re-tokenizes it all over again. A commit only
// moves the git branch ref: the contents are already in the index from put's
// incremental add, so rebuilding the world on every commit was an O(N^2) trap
// that took 150+ seconds per commit during a bulk import.
func TestCommitIsFastAndPreservesSearchability(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// Put an entry into a branch.
	if _, err := callMCP(t, srv, tok, "put blackegg/me@fact -- test-commit-speed-marker-unique"); err != nil {
		t.Fatal(err)
	}

	// Commit must be sub-second, not hundreds of seconds.
	t0 := time.Now()
	out, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed")
	if err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	el := time.Since(t0)
	if el > 3*time.Second {
		t.Fatalf("commit took too long (%v); it should not do a full reload", el)
	}
	if !strings.Contains(out, "committed") && !strings.Contains(out, "nothing pending") {
		t.Fatalf("unexpected commit output: %s", out)
	}

	// Search must immediately find the entry we just committed.
	res, err := callMCP(t, srv, tok, "recall test-commit-speed-marker-unique --scope blackegg/me@fact")
	if err != nil {
		t.Fatalf("recall failed: %v", err)
	}
	if !strings.Contains(res, "test-commit-speed-marker-unique") {
		t.Fatalf("entry should remain searchable after commit, got:\n%s", res)
	}
}

// Committing one branch must not disturb other branches.
func TestCommitPreservesOtherBranches(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	for _, b := range []string{"fact", "record"} {
		if _, err := callMCP(t, srv, tok, fmt.Sprintf("put blackegg/me@%s -- marker-in-%s", b, b)); err != nil {
			t.Fatal(err)
		}
	}

	// Commit only one of them.
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m fact"); err != nil {
		t.Fatal(err)
	}

	// Both should still be searchable.
	for _, b := range []string{"fact", "record"} {
		res, err := callMCP(t, srv, tok, fmt.Sprintf("recall marker-in-%s --scope blackegg/me@%s", b, b))
		if err != nil {
			t.Fatalf("recall %s failed: %v", b, err)
		}
		if !strings.Contains(res, "marker-in-"+b) {
			t.Fatalf("branch %s content not found after committing fact", b)
		}
	}
}
