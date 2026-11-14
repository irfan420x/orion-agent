// rc25 tests: F-A19-2 — `orion backup` honours ORION_DATA_DIR in
// the corrupt-config fallback, and `oriond --restore` warns clearly
// that the file's data_dir is unknown.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCmdBackupCorruptConfigHonoursDataDir (F-A19-2, rc25): a corrupt
// config must not make `orion backup` silently target ~/.orion
// while the daemon runs on another dir — the fallback honours
// ORION_DATA_DIR, snapshots that dir, and says so on stderr.
func TestCmdBackupCorruptConfigHonoursDataDir(t *testing.T) {
	home := scratchHome(t)
	custom := filepath.Join(home, "custom-orion")
	if err := os.MkdirAll(custom, 0o700); err != nil {
		t.Fatalf("mkdir custom data dir: %v", err)
	}
	for _, f := range []string{"memory.json", "sessions.json"} {
		if err := os.WriteFile(filepath.Join(custom, f), []byte(`{"seed": true}`+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if err := os.WriteFile(filepath.Join(custom, "marker.txt"), []byte("custom-dir\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	// The default dir holds ONLY the corrupt config — if the
	// fallback ignored ORION_DATA_DIR, the snapshot would be of
	// this dir (and would contain the corrupt config, not the
	// marker).
	defDir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(defDir, 0o700); err != nil {
		t.Fatalf("mkdir default data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(defDir, "config.json"), []byte("{this is not json"), 0o600); err != nil {
		t.Fatalf("write corrupt config: %v", err)
	}
	t.Setenv("ORION_DATA_DIR", custom)

	_, stderr, err := captureOutput(func() error { return cmdBackup(nil) })
	if err != nil {
		t.Fatalf("cmdBackup with corrupt config + ORION_DATA_DIR: %v (stderr: %s)", err, stderr)
	}
	if !strings.Contains(stderr, "data dir from ORION_DATA_DIR") || !strings.Contains(stderr, custom) {
		t.Errorf("stderr = %q, want the ORION_DATA_DIR fallback named with the custom dir", stderr)
	}
	snaps := findSnapshots(t, custom)
	if len(snaps) != 1 {
		t.Fatalf("want exactly 1 snapshot, got %d (%v)", len(snaps), snaps)
	}
	if got, err := os.ReadFile(filepath.Join(snaps[0], "marker.txt")); err != nil || string(got) != "custom-dir\n" {
		t.Errorf("snapshot marker.txt = %q (err %v), want the custom dir's marker — the backup targeted the wrong dir", got, err)
	}
	if _, err := os.Stat(filepath.Join(snaps[0], "config.json")); err == nil {
		t.Errorf("snapshot contains config.json, but the custom dir has none — the backup targeted ~/.orion, not ORION_DATA_DIR")
	}
}
