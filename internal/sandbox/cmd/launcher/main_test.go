package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// findGo returns a go toolchain binary: GOROOT's first (the same
// one the tests were built with), then PATH, then the per-user
// install this VM is known to use. It skips the test when none is
// found instead of failing a checkout that simply has no toolchain.
func findGo(t *testing.T) string {
	t.Helper()
	if goBin := filepath.Join(runtime.GOROOT(), "bin", "go"); fileExists(goBin) {
		return goBin
	}
	if goBin, err := exec.LookPath("go"); err == nil {
		return goBin
	}
	if home, err := os.UserHomeDir(); err == nil {
		if goBin := filepath.Join(home, "go1.27.4", "bin", "go"); fileExists(goBin) {
			return goBin
		}
	}
	t.Skip("no go toolchain found (GOROOT, PATH, ~/go1.27.4/bin/go)")
	return ""
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// repoRoot walks up from this file until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("could not find repo root (go.mod) above %s", file)
		}
		root = parent
	}
}

// TestLauncherBuildsFromSource proves the orion-seccomp launcher
// builds from source with the exact flags `make build` uses
// (CGO_ENABLED=0, -trimpath) and that the built binary works: no
// arguments is a usage error, and /bin/true runs under the
// installed filter. This is the build half of the default-on
// guarantee — a fresh `make build` must produce a working
// launcher, because with the sandbox on by default a missing or
// broken launcher makes every shell_exec fail closed.
func TestLauncherBuildsFromSource(t *testing.T) {
	goBin := findGo(t)
	root := repoRoot(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "orion-seccomp")
	cmd := exec.Command(goBin, "build", "-trimpath", "-o", out, "./internal/sandbox/cmd/launcher")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("build orion-seccomp: %v\n%s", err, buf.String())
	}
	if fi, err := os.Stat(out); err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("built launcher %s is not an executable file: %v", out, err)
	}

	noArgs := exec.Command(out)
	if err := noArgs.Run(); err == nil {
		t.Fatal("launcher with no arguments: want a usage error, got exit 0")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("launcher with no arguments: want exit code 2, got %v", err)
	}

	trueCmd := exec.Command(out, "/bin/true")
	trueCmd.Env = []string{"PATH=/usr/bin:/bin"}
	if out, err := trueCmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher /bin/true: want exit 0 (filter installs), got %v: %s", err, strings.TrimSpace(string(out)))
	}
}
