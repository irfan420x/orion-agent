// rc25 tests: F-A19-2 (restore half) — the corrupt-config fallback
// must say plainly that the file's data_dir is unknown, and honour
// ORION_DATA_DIR instead of silently restoring into ~/.orion.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/backup"
)

// TestRestoreWithCorruptConfigHonoursDataDir: a corrupt config must
// not block a restore, and must not make it silently target
// ~/.orion while the daemon runs on another dir. With ORION_DATA_DIR
// set, the restore targets that dir and the warning names it — and
// says the config file's data_dir is unknown.
func TestRestoreWithCorruptConfigHonoursDataDir(t *testing.T) {
	scratchHome(t)
	home := os.Getenv("HOME")
	custom := filepath.Join(home, "custom-orion")
	if err := os.MkdirAll(custom, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "marker.txt"), []byte("before-restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(custom, filepath.Join(home, ".orion-backups"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// The config Load reads is the default path (~/.orion) — corrupt
	// it, so the fallback runs; the data dir itself is the custom one.
	defDir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(defDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defDir, "config.json"), []byte("{corrupt json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Damage the marker so the restore has something to put back.
	if err := os.WriteFile(filepath.Join(custom, "marker.txt"), []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORION_DATA_DIR", custom)

	code, out := captureRestoreRun([]string{"--restore", snap})
	if code != 0 {
		t.Fatalf("--restore with corrupt config + ORION_DATA_DIR: exit %d, want 0; output:\n%s", code, out)
	}
	for _, want := range []string{"cannot load config", "data_dir is unknown", "data dir from ORION_DATA_DIR", custom} {
		if !strings.Contains(out, want) {
			t.Errorf("--restore output does not say %q:\n%s", want, out)
		}
	}
	marker, err := os.ReadFile(filepath.Join(custom, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "before-restore\n" {
		t.Errorf("marker after restore = %q, want %q (the restore must target ORION_DATA_DIR, not ~/.orion)", marker, "before-restore\n")
	}
}
