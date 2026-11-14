// drain_timeout_test.go — F-A17-17 residual (rc24): pin the drain
// bound at 30s in BOTH entrypoints.
//
// cmd/oriond/main.go and cmd/orion/main.go are package main in
// different binaries, so no cross-package import can share the
// constant; the two `const drainTimeout` declarations are kept in
// sync only by their cross-reference comments. This test reads both
// source files and asserts both declare the same value, so changing
// one without the other fails loudly instead of silently reopening
// the 10s-vs-30s disagreement rc23 fixed.
//
// The test parses the declarations (it does not execute either
// binary), so it is cheap and has no effect on the live daemon.
package orion_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// drainTimeoutDecl matches `const drainTimeout = <n> * time.<Unit>`
// and captures the number and the time unit.
var drainTimeoutDecl = regexp.MustCompile(
	`const\s+drainTimeout\s*=\s*(\d+)\s*\*\s*time\.(Second|Millisecond|Minute|Hour)`)

func drainTimeoutFromFile(t *testing.T, path string) time.Duration {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := drainTimeoutDecl.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s: no `const drainTimeout = <n> * time.<Unit>` declaration found — "+
			"the drain bound must stay a single named const in each entrypoint", path)
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("%s: drainTimeout number %q is not an integer: %v", path, m[1], err)
	}
	var unit time.Duration
	switch string(m[2]) {
	case "Millisecond":
		unit = time.Millisecond
	case "Second":
		unit = time.Second
	case "Minute":
		unit = time.Minute
	case "Hour":
		unit = time.Hour
	}
	// The const must also be used: a declaration nothing calls is a
	// dead pin. Both entrypoints drain through a.Drain(drainTimeout).
	if !strings.Contains(string(raw), "Drain(drainTimeout)") {
		t.Errorf("%s: drainTimeout is declared but never passed to Drain — "+
			"the pin is dead; wire the const into the shutdown path", path)
	}
	return time.Duration(n) * unit
}

func TestDrainTimeoutEqualAcrossEntrypoints(t *testing.T) {
	root := installTestRepoRoot(t)
	oriond := filepath.Join(root, "cmd", "oriond", "main.go")
	orion := filepath.Join(root, "cmd", "orion", "main.go")

	dOriond := drainTimeoutFromFile(t, oriond)
	dOrion := drainTimeoutFromFile(t, orion)

	if dOriond != dOrion {
		t.Fatalf("drainTimeout disagrees: cmd/oriond/main.go=%s, cmd/orion/main.go=%s — "+
			"change BOTH entrypoints together (F-A17-17)", dOriond, dOrion)
	}
	if dOriond != 30*time.Second {
		t.Fatalf("drainTimeout is %s, want 30s in both entrypoints — 30s is the "+
			"F-A10-5 margin that keeps a turn from racing transcript/memory "+
			"writes during teardown; if the bound must change, change it in "+
			"both files and update this test with the reason", dOriond)
	}
}
