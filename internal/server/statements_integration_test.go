package server

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// A single DSL line can chain multiple statements separated by ';' or '\n'.
//
// This is how layered memory works efficiently: asking fact, record and thought
// separately takes three recall queries, and paying three round trips for every
// question would be prohibitive. Chaining runs them in one turn, in order, and
// returns each block with a label identifying the statement.
func TestChainedRecallAcrossBranches(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:  "zero",
		Name: "test",
		Cap:  "rwx",
	})

	// Seed three separate branches under the same project.
	seed := func(branch, content string) {
		t.Helper()
		cmd := "put blackegg/me@" + branch + " -- " + content
		if _, err := callMCP(t, srv, tok, cmd); err != nil {
			t.Fatalf("seed %s: %v", branch, err)
		}
		if _, err := callMCP(t, srv, tok, "commit blackegg/me@"+branch+" -m seed"); err != nil {
			t.Fatalf("commit %s: %v", branch, err)
		}
	}
	seed("fact", "architecture decision: git-backed storage")
	seed("record", "conversation: user asked how memory is laid out")
	seed("thought", "internal reasoning: maybe a sqlite backend is easier")

	// One call, three scopes, ordered fact -> record -> thought.
	query := "recall memory --scope blackegg/me@fact ; " +
		"recall memory --scope blackegg/me@record ; " +
		"recall memory --scope blackegg/me@thought"

	out, err := callMCP(t, srv, tok, query)
	if err != nil {
		t.Fatalf("chained recall failed: %v", err)
	}

	// Output must show the three statements in order with their labels.
	for _, want := range []string{
		"[statement] recall memory --scope blackegg/me@fact",
		"[statement] recall memory --scope blackegg/me@record",
		"[statement] recall memory --scope blackegg/me@thought",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output is missing statement label %q:\n%s", want, out)
		}
	}

	// Verify order: fact before record, record before thought.
	posFact := strings.Index(out, "scope blackegg/me@fact")
	posRecord := strings.Index(out, "scope blackegg/me@record")
	posThought := strings.Index(out, "scope blackegg/me@thought")
	if posFact < 0 || posRecord < 0 || posThought < 0 ||
		posFact >= posRecord || posRecord >= posThought {
		t.Fatalf("results are out of order:\n%s", out)
	}
}

// Chained execution stops at the first failure: later statements do not run.
//
// If a write fails, a following commit must not create an empty or half-baked
// commit. The error must name which statement broke.
func TestChainedExecutionStopsOnError(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{
		Sub:  "zero",
		Name: "test",
		Cap:  "rwx",
	})

	// Third statement is a typo'd verb that fails.
	cmd := "recall a ; recall b ; notaverb something ; recall c"
	_, err := callMCP(t, srv, tok, cmd)
	if err == nil {
		t.Fatal("expected chained execution to fail on unknown verb")
	}
	msg := err.Error()
	if !strings.Contains(msg, "statement 3 of 4") {
		t.Fatalf("error should name the failing statement index, got %q", msg)
	}
	if !strings.Contains(msg, "notaverb") {
		t.Fatalf("error should name the failing verb, got %q", msg)
	}
}
