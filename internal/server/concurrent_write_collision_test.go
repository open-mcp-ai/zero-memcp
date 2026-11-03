package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// When many memories are written in parallel, none may collide and overwrite
// each other.
//
// The timestamp resolution used to be milliseconds (HHMMSS.mmm). Two writes in
// the same millisecond picked the exact same filename; the second one silently
// overwrote the first in the dirty overlay. A batch import of 100 items dropped
// ~12 of them silently without any error reported.
func TestConcurrentWritesDoNotCollide(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	const n = 60
	var wg sync.WaitGroup
	wg.Add(n)

	addrs := make([]string, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			cmd := fmt.Sprintf("put blackegg/me@fact/2026-10-24 -- item-%d-unique-payload", idx)
			out, err := callMCP(t, srv, tok, cmd)
			if err != nil {
				errs[idx] = err
				return
			}
			// Extract the reported path
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "ok ") {
					addrs[idx] = strings.TrimPrefix(line, "ok ")
					break
				}
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	seen := map[string]int{}
	for i, addr := range addrs {
		if addr == "" {
			t.Fatalf("write %d reported no address", i)
		}
		if prev, ok := seen[addr]; ok {
			t.Fatalf("address collision between write %d and write %d: %s", prev, i, addr)
		}
		seen[addr] = i
	}

	if len(seen) != n {
		t.Fatalf("expected %d distinct addresses, got %d", n, len(seen))
	}

	// Commit and ensure all n entries are accounted for.
	out, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed")
	if err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf("%d entries", n)) {
		t.Fatalf("commit should report %d entries, got: %s", n, out)
	}
}
