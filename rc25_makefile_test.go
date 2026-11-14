// rc25 tests: F-A19-7 — the Makefile's Go toolchain fallback must
// include the audit toolchain path, and Go-free targets (clean,
// version) must work on a Go-less host instead of dying at parse
// time on the $(error).
//
// The Go-less host is simulated with GO=/nonexistent/go on the
// command line: a command-line GO cannot be reassigned by the
// fallback chain, so it stays unrunnable — exactly the state a
// host with no working Go candidate reaches. The Makefile is
// copied to a scratch dir so `make clean` never touches the real
// bin/ (the live binaries) and `make version` never needs the
// repo's version.go (it degrades to empty/unknown there, which is
// fine — the test only asserts the target runs).
package orion_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeTestRun runs `make -f <makefile> <args...>` in dir and returns
// its combined output and exit code.
func makeTestRun(t *testing.T, dir, makefile string, args ...string) (string, int) {
	t.Helper()
	argv := append([]string{"-f", makefile}, args...)
	cmd := exec.Command("make", argv...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run make %v: %v", argv, err)
		}
	}
	return string(out), code
}

func TestMakefileGoFallback(t *testing.T) {
	repoRoot := installTestRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	// The audit toolchain path must be a candidate (and named in
	// the parse-time error, so the operator knows it was checked).
	if !strings.Contains(string(raw), "workspace/orion-audit/toolchain/go/bin/go") {
		t.Error("Makefile does not list the audit toolchain path (~/workspace/orion-audit/toolchain/go/bin/go) as a Go candidate")
	}

	dir := t.TempDir()
	makefile := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(makefile, raw, 0o644); err != nil {
		t.Fatalf("copy Makefile: %v", err)
	}
	// A broken GO on the command line simulates the Go-less host.
	broken := "GO=/nonexistent/go"

	if out, code := makeTestRun(t, dir, makefile, broken, "clean"); code != 0 {
		t.Errorf("make clean with a broken GO: exit %d, want 0 (clean is Go-free):\n%s", code, out)
	}
	if out, code := makeTestRun(t, dir, makefile, broken, "version"); code != 0 {
		t.Errorf("make version with a broken GO: exit %d, want 0 (version is Go-free):\n%s", code, out)
	} else if !strings.Contains(out, "orion") {
		t.Errorf("make version output = %q, want it to name orion", out)
	}
	if out, code := makeTestRun(t, dir, makefile, broken, "test"); code == 0 {
		t.Errorf("make test with a broken GO: exit 0, want non-zero (test needs Go):\n%s", out)
	} else if !strings.Contains(out, "Go toolchain not found") {
		t.Errorf("make test with a broken GO did not fail with the toolchain error:\n%s", out)
	}
	// Bare `make` is the default target (release-check), which
	// needs Go — it must still fail with the toolchain error.
	if out, code := makeTestRun(t, dir, makefile, broken); code == 0 {
		t.Errorf("bare make with a broken GO: exit 0, want non-zero (the default target needs Go):\n%s", out)
	} else if !strings.Contains(out, "Go toolchain not found") {
		t.Errorf("bare make with a broken GO did not fail with the toolchain error:\n%s", out)
	}
}
