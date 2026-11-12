package main

import (
	"io"
	"os"
	"testing"
)

// quiet runs fn with stdout/stderr discarded so daemon output does not
// spam the test log.
func quietRun(args []string) int {
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout, os.Stderr = w, w
	code := run(args)
	_ = w.Close()
	_, _ = io.Copy(io.Discard, r)
	os.Stdout, os.Stderr = oldOut, oldErr
	return code
}

func scratchHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ORION_TELEGRAM_TOKEN", "")
	t.Setenv("ORION_API_KEY", "")
	t.Setenv("ORION_PROVIDER", "")
	t.Setenv("ORION_MODEL", "")
	t.Setenv("ORION_DATA_DIR", "")
}

func TestRunHelpAndVersion(t *testing.T) {
	if code := quietRun([]string{"--help"}); code != 0 {
		t.Errorf("--help: exit %d, want 0", code)
	}
	if code := quietRun([]string{"-h"}); code != 0 {
		t.Errorf("-h: exit %d, want 0", code)
	}
	if code := quietRun([]string{"--version"}); code != 0 {
		t.Errorf("--version: exit %d, want 0", code)
	}
	if code := quietRun([]string{"-v"}); code != 0 {
		t.Errorf("-v: exit %d, want 0", code)
	}
	if code := quietRun([]string{"version"}); code != 0 {
		t.Errorf("version: exit %d, want 0", code)
	}
}

func TestRunRejectsBadArgs(t *testing.T) {
	if code := quietRun([]string{"bogus"}); code != 2 {
		t.Errorf("bogus arg: exit %d, want 2", code)
	}
	if code := quietRun([]string{"--help", "extra"}); code != 2 {
		t.Errorf("--help extra: exit %d, want 2", code)
	}
	if code := quietRun([]string{"--version", "extra"}); code != 2 {
		t.Errorf("--version extra: exit %d, want 2", code)
	}
}

func TestRunNoTokenFailsGracefully(t *testing.T) {
	scratchHome(t)
	// No token configured: daemon must exit 1 with a clear error, not
	// panic and not attempt any network call.
	if code := quietRun(nil); code != 1 {
		t.Errorf("no token: exit %d, want 1", code)
	}
}
