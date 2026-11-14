package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/backup"
	"github.com/irfan/orion/internal/messaging"
)

// F-A18-2 / F-A18-7 (rc24): --restore takes the instance lock before
// restoring (a running daemon would undo it), falls back to the
// default data dir when the config is corrupt, and closes a
// resurrected allow-all window from the snapshot.

// captureRestoreRun runs run(args) with stdout/stderr captured and returns
// the exit code and the combined output. No daemon runs concurrently
// in these tests, so swapping the globals cannot race a live writer
// (the rc23 second-instance test's warning does not apply here).
func captureRestoreRun(args []string) (int, string) {
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout, os.Stderr = w, w
	code := run(args)
	_ = w.Close()
	out, _ := io.ReadAll(r)
	os.Stdout, os.Stderr = oldOut, oldErr
	return code, string(out)
}

// TestRestoreRefusesWhileLockHeld: with the instance lock held (a
// daemon running over the same data dir), --restore refuses with
// exit 2 and tells the operator to stop the daemon first — instead
// of restoring and letting the daemon undo it on exit.
func TestRestoreRefusesWhileLockHeld(t *testing.T) {
	scratchHome(t)
	home := os.Getenv("HOME")
	dataDir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := acquireInstanceLock(dataDir)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer held.Close()
	code, out := captureRestoreRun([]string{"--restore", filepath.Join(t.TempDir(), "no-such-snapshot")})
	if code != exitAlreadyRunning {
		t.Errorf("--restore while locked: exit %d, want %d", code, exitAlreadyRunning)
	}
	for _, want := range []string{"already running", "stop the daemon first"} {
		if !strings.Contains(out, want) {
			t.Errorf("--restore while locked: output does not say %q:\n%s", want, out)
		}
	}
}

// TestRestoreWithCorruptConfigFallsBackToDefaults: a corrupt
// config.json must not block a restore — the operator restores
// precisely because the data dir is damaged. The restore falls back
// to the default data dir (~/.orion) with a warning, and the good
// config comes back with the snapshot.
func TestRestoreWithCorruptConfigFallsBackToDefaults(t *testing.T) {
	scratchHome(t)
	home := os.Getenv("HOME")
	dataDir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	goodCfg := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA","telegram_allowed_chats":[42],"data_dir":"` + dataDir + `"}`
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(goodCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "marker.txt"), []byte("before-restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(dataDir, filepath.Join(home, ".orion-backups"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Damage the data dir: corrupt the config, clobber the marker.
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte("{corrupt json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "marker.txt"), []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := captureRestoreRun([]string{"--restore", snap})
	if code != 0 {
		t.Fatalf("--restore with corrupt config: exit %d, want 0; output:\n%s", code, out)
	}
	for _, want := range []string{"cannot load config", "default data dir"} {
		if !strings.Contains(out, want) {
			t.Errorf("--restore with corrupt config: output does not say %q:\n%s", want, out)
		}
	}
	marker, err := os.ReadFile(filepath.Join(dataDir, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "before-restore\n" {
		t.Errorf("marker after restore = %q, want %q", marker, "before-restore\n")
	}
	restoredCfg, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(restoredCfg) != goodCfg {
		t.Errorf("config after restore = %q, want the good config back", restoredCfg)
	}
	// The restore released the lock: the next instance can start.
	next, err := acquireInstanceLock(dataDir)
	if err != nil {
		t.Fatalf("lock after restore: %v", err)
	}
	_ = next.Close()
}

// restoreAllowAllFixture builds a scratch home whose data dir holds
// a marker file and (unless state is "") an allow-all-until state
// file, snapshots it, then damages the marker — so a restore has
// something visible to put back. The snapshot also contains an
// oriond.lock (the data dir had one at snapshot time) — but Restore
// never copies it over the live lock (F-A19-5), so the post-restore
// lock refresh is a no-op here: it re-checks the path and finds the
// same file still locked.
func restoreAllowAllFixture(t *testing.T, state string) (home, snap string) {
	t.Helper()
	scratchHome(t)
	home = os.Getenv("HOME")
	dataDir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "marker.txt"), []byte("before-restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if err := os.WriteFile(filepath.Join(dataDir, messaging.AllowAllStateFileName), []byte(state+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, "oriond.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(dataDir, filepath.Join(home, ".orion-backups"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "marker.txt"), []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, snap
}

// TestRestoreNeutralizesFutureAllowAll: a snapshot taken while the
// allow-all window was open must not reopen the bot to every chat on
// the next start. A future deadline is closed (rewritten in the
// past); a past, corrupt, or missing state file is left alone.
func TestRestoreNeutralizesFutureAllowAll(t *testing.T) {
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	cases := []struct {
		name       string
		state      string // "" = no state file, else its content
		wantClosed bool   // restore warns it closed a future window
		wantState  string // "" = file must stay absent; else exact content after restore
		wantPast   bool   // state file must parse to a past deadline
	}{
		{"future deadline is closed", future, true, "", true},
		{"past deadline is left alone", past, false, past + "\n", false},
		{"corrupt state is left alone", "not-a-deadline", false, "not-a-deadline\n", false},
		{"missing state stays missing", "", false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, snap := restoreAllowAllFixture(t, tc.state)
			dataDir := filepath.Join(home, ".orion")
			code, out := captureRestoreRun([]string{"--restore", snap})
			if code != 0 {
				t.Fatalf("--restore: exit %d, want 0; output:\n%s", code, out)
			}
			if tc.wantClosed && !strings.Contains(out, "future allow-all window") {
				t.Errorf("output does not say the future window was closed:\n%s", out)
			}
			if !tc.wantClosed && strings.Contains(out, "future allow-all window") {
				t.Errorf("output claims a future window was closed, want no such warning:\n%s", out)
			}
			marker, err := os.ReadFile(filepath.Join(dataDir, "marker.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(marker) != "before-restore\n" {
				t.Errorf("marker after restore = %q, want %q", marker, "before-restore\n")
			}
			raw, err := os.ReadFile(filepath.Join(dataDir, messaging.AllowAllStateFileName))
			if tc.wantState == "" && !tc.wantPast {
				if err == nil {
					t.Errorf("allow-all state file exists after restore, want it absent (got %q)", raw)
				} else if !os.IsNotExist(err) {
					t.Fatalf("read allow-all state: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read allow-all state: %v", err)
			}
			if tc.wantPast {
				deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
				if err != nil {
					t.Fatalf("restored allow-all state %q does not parse: %v", raw, err)
				}
				if !deadline.Before(time.Now()) {
					t.Errorf("restored allow-all deadline = %v, want it in the past", deadline)
				}
				return
			}
			if string(raw) != tc.wantState {
				t.Errorf("allow-all state after restore = %q, want %q (left alone)", raw, tc.wantState)
			}
		})
	}
}

// TestRefreshInstanceLock: refreshInstanceLock re-takes the lock
// when the lock file was replaced out from under the holder. With
// no replacement the same file comes back still held; after a
// temp-file + rename (done here by hand — Restore itself never
// replaces oriond.lock, F-A19-5) the old lock is stale — a new
// locker succeeds on the new file — until refresh re-takes it.
func TestRefreshInstanceLock(t *testing.T) {
	dir := t.TempDir()
	f, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	same, err := refreshInstanceLock(f, dir)
	if err != nil {
		t.Fatalf("refresh without replacement: %v", err)
	}
	if same != f {
		t.Error("refresh without replacement: got a different file, want the same one")
	}
	if _, err := acquireInstanceLock(dir); err == nil {
		t.Error("lock not held after no-op refresh: second locker succeeded")
	}
	// Replace the lock file out from under the holder.
	tmp := filepath.Join(dir, "oriond.lock.tmp")
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "oriond.lock")); err != nil {
		t.Fatal(err)
	}
	stale, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("lock on replaced file: %v (want success — the old lock is stale)", err)
	}
	_ = stale.Close()
	refreshed, err := refreshInstanceLock(f, dir)
	if err != nil {
		t.Fatalf("refresh after replacement: %v", err)
	}
	if refreshed == f {
		t.Error("refresh after replacement: got the same file, want a new one on the current path")
	}
	if _, err := acquireInstanceLock(dir); err == nil {
		t.Error("lock not held on the current file after refresh: second locker succeeded")
	}
	_ = refreshed.Close()
	// Released: the next instance can start.
	next, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = next.Close()
}
