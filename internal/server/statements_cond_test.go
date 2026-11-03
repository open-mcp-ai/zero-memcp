package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// "||" falls through to the next layer when this one finds nothing.
//
// This is the shape layering needs: ask the top layer, and only pay for the next
// one when the answer was empty. It is NOT shell's '||' - a query that finds
// nothing exits successfully here, so keying the fallback on errors would make it
// never fire.
func TestOrFallsThroughOnEmptyResult(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// Only the second and third layers have content.
	seed := func(branch, content string) {
		t.Helper()
		if _, err := callMCP(t, srv, tok, "put blackegg/me@"+branch+" -- "+content); err != nil {
			t.Fatal(err)
		}
		if _, err := callMCP(t, srv, tok, "commit blackegg/me@"+branch+" -m seed"); err != nil {
			t.Fatal(err)
		}
	}
	seed("record", "conversation about zqxkeyword and its consequences")
	seed("thought", "reasoning about zqxkeyword")

	chain := "recall zqxkeyword --scope blackegg/me@fact || " +
		"recall zqxkeyword --scope blackegg/me@record || " +
		"recall zqxkeyword --scope blackegg/me@thought"

	out, err := callMCP(t, srv, tok, chain)
	if err != nil {
		t.Fatalf("chain failed: %v", err)
	}

	// The fact layer was empty, so the record layer ran.
	if !strings.Contains(out, "scope blackegg/me@record") {
		t.Fatalf("record layer should have run after an empty fact layer:\n%s", out)
	}
	// The record layer found something, so the thought layer must be skipped -
	// that is the saving the operator is buying with '||'.
	if strings.Contains(out, "scope blackegg/me@thought") {
		t.Fatalf("thought layer must be skipped once record answered:\n%s", out)
	}
}

// When the first layer answers, the rest of the chain is skipped.
func TestOrStopsAtFirstHit(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	seed := func(branch, content string) {
		t.Helper()
		if _, err := callMCP(t, srv, tok, "put blackegg/me@"+branch+" -- "+content); err != nil {
			t.Fatal(err)
		}
		if _, err := callMCP(t, srv, tok, "commit blackegg/me@"+branch+" -m seed"); err != nil {
			t.Fatal(err)
		}
	}
	seed("fact", "decision about qqkeyword storage")
	seed("record", "conversation about qqkeyword storage")
	seed("thought", "thinking about qqkeyword storage")

	chain := "recall qqkeyword --scope blackegg/me@fact || " +
		"recall qqkeyword --scope blackegg/me@record || " +
		"recall qqkeyword --scope blackegg/me@thought"

	out, err := callMCP(t, srv, tok, chain)
	if err != nil {
		t.Fatalf("chain failed: %v", err)
	}

	if !strings.Contains(out, "scope blackegg/me@fact") {
		t.Fatalf("fact layer should have run:\n%s", out)
	}
	// The rest must not have run: that is the entire saving.
	if strings.Contains(out, "scope blackegg/me@record") {
		t.Fatalf("record layer must be skipped once fact answered:\n%s", out)
	}
	if strings.Contains(out, "scope blackegg/me@thought") {
		t.Fatalf("thought layer must be skipped once fact answered:\n%s", out)
	}
}

// "&&" runs the next statement only when the previous one produced something.
func TestAndRequiresPreviousHit(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// Nothing is seeded: the recall is empty, so the follow-up must not run.
	out, err := callMCP(t, srv, tok,
		"recall nokeywordhere --scope blackegg/me@fact && status")
	if err != nil {
		t.Fatalf("chain failed: %v", err)
	}
	if strings.Contains(out, "docs:") {
		t.Fatalf("'status' must be skipped when the recall found nothing:\n%s", out)
	}

	// With a hit, the follow-up runs.
	if _, err := callMCP(t, srv, tok, "put blackegg/me@fact -- findable text here"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
		t.Fatal(err)
	}
	out, err = callMCP(t, srv, tok,
		"recall findable --scope blackegg/me@fact && status")
	if err != nil {
		t.Fatalf("chain failed: %v", err)
	}
	if !strings.Contains(out, "docs:") {
		t.Fatalf("'status' should run after a hit:\n%s", out)
	}
}

// A permission or syntax error is fatal even with '||'.
//
// Falling through a permission error would answer with memory from a layer the
// caller was never allowed to reach, looking like a normal result.
func TestOrDoesNotSwallowErrors(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:   "zero",
		Name:  "restricted",
		Cap:   "rwx",
		Mount: []auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "fact"}},
	})

	// Two statements, so the chain runs: the first writes outside this
	// credential's mount table and must abort rather than fall through.
	chain := "put blackegg/daily@zero -- nope || status"
	_, err := callMCP(t, srv, tok, chain)
	if err == nil {
		t.Fatal("a permission error must abort the chain, not fall through")
	}
	if !strings.Contains(err.Error(), "outside this credential") {
		t.Fatalf("error should be the permission failure, got %q", err)
	}
	if strings.Contains(err.Error(), "statement ") {
		t.Fatalf("a single-statement failure should not be wrapped with an index, got %q", err)
	}
}

// When several statements are chained, the failing one is named.
func TestChainErrorNamesStatement(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	_, err := callMCP(t, srv, tok, "status ; notaverb ; status")
	if err == nil {
		t.Fatal("unknown verb must fail")
	}
	if !strings.Contains(err.Error(), "statement 2 of 3") {
		t.Fatalf("error should name the failing statement index, got %q", err)
	}
}

// Semicolons still run every statement regardless of results.
func TestSemicolonAlwaysRuns(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	out, err := callMCP(t, srv, tok,
		"recall nokeywordhere --scope blackegg/me@fact ; status")
	if err != nil {
		t.Fatalf("chain failed: %v", err)
	}
	if !strings.Contains(out, "docs:") {
		t.Fatalf("';' must run the next statement even after an empty recall:\n%s", out)
	}
}
