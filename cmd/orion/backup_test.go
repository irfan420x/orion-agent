package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureOutput runs fn with stdout and stderr redirected to pipes
// and returns what each captured, plus fn's error.
func captureOutput(fn func() error) (string, string, error) {
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	err := fn()
	_ = outW.Close()
	_ = errW.Close()
	outBuf := make([]byte, 0, 4096)
	errBuf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, rerr := outR.Read(tmp)
		outBuf = append(outBuf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	for {
		n, rerr := errR.Read(tmp)
		errBuf = append(errBuf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	os.Stdout, os.Stderr = oldOut, oldErr
	return string(outBuf), string(errBuf), err
}

// seedScratchDataDir creates a fake ~/.orion under the scratch HOME
// with a couple of state files, returning the data dir path.
func seedScratchDataDir(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir scratch data dir: %v", err)
	}
	for _, f := range []string{"memory.json", "sessions.json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(`{"seed": true}`+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return dir
}

// findSnapshots returns the snapshot dirs under the default backup
// root for dir.
func findSnapshots(t *testing.T, dir string) []string {
	t.Helper()
	root := filepath.Join(filepath.Dir(dir), ".orion-backups")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read backup root %s: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out
}

// TestCmdBackupCorruptConfig (F-A18-7, rc24): a corrupt config.json
// must not block `orion backup`. The command warns on stderr and
// falls back to the default data dir (~/.orion), snapshots it, and
// the snapshot preserves the corrupt config itself for inspection.
// Only scratch dirs are used; the real ~/.orion is never touched.
func TestCmdBackupCorruptConfig(t *testing.T) {
	home := scratchHome(t)
	dir := seedScratchDataDir(t, home)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{this is not json"), 0o600); err != nil {
		t.Fatalf("write corrupt config: %v", err)
	}
	stdout, stderr, err := captureOutput(func() error { return cmdBackup(nil) })
	if err != nil {
		t.Fatalf("cmdBackup with corrupt config: %v (stderr: %s)", err, stderr)
	}
	if !strings.Contains(stderr, "cannot load config") || !strings.Contains(stderr, "using default data dir") {
		t.Errorf("stderr = %q, want a visible config-load warning + default-data-dir fallback", stderr)
	}
	if !strings.Contains(stderr, filepath.Join(home, ".orion", "config.json")) {
		t.Errorf("stderr = %q, want the corrupt config path named", stderr)
	}
	snaps := findSnapshots(t, dir)
	if len(snaps) != 1 {
		t.Fatalf("want exactly 1 snapshot, got %d (%v)", len(snaps), snaps)
	}
	if !strings.Contains(stdout, "snapshot: ") || !strings.Contains(stdout, snaps[0]) {
		t.Errorf("stdout = %q, want the snapshot path %q", stdout, snaps[0])
	}
	// The snapshot holds the state files AND the corrupt config.
	for _, f := range []string{"memory.json", "sessions.json", "config.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(snaps[0], f)); err != nil {
			t.Errorf("snapshot is missing %s: %v", f, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(snaps[0], "config.json")); err != nil || string(got) != "{this is not json" {
		t.Errorf("snapshot config.json = %q (err %v), want the corrupt original preserved", got, err)
	}
}

// TestCmdBackupMissingConfig: no config.json at all still backs up
// (Load falls back to defaults internally). Regression guard for
// the F-A18-7 fallback — the corrupt-config path must behave like
// the missing-config path.
func TestCmdBackupMissingConfig(t *testing.T) {
	home := scratchHome(t)
	dir := seedScratchDataDir(t, home)
	if _, stderr, err := captureOutput(func() error { return cmdBackup(nil) }); err != nil {
		t.Fatalf("cmdBackup with missing config: %v (stderr: %s)", err, stderr)
	}
	if snaps := findSnapshots(t, dir); len(snaps) != 1 {
		t.Fatalf("want exactly 1 snapshot, got %d (%v)", len(snaps), snaps)
	}
}

// TestCmdBackupHelpMentionsLiveSnapshot (F-A18-8, rc24): the help
// text must say the snapshot is live, not quiesced, so an operator
// learns the limit before running it.
func TestCmdBackupHelpMentionsLiveSnapshot(t *testing.T) {
	stdout, _, err := captureOutput(func() error { return cmdBackup([]string{"--help"}) })
	if err != nil {
		t.Fatalf("cmdBackup --help: %v", err)
	}
	if !strings.Contains(stdout, "live, not quiesced") || !strings.Contains(stdout, "mix moments") {
		t.Errorf("--help output = %q, want the live-snapshot warning", stdout)
	}
}
