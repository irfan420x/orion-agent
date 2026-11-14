package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rc23W3SeedDataDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{
		"config.json":         `{"provider": "deepseek", "model": "deepseek-chat"}`,
		"permissions.json":    `{"version": 2, "rules": {"shell_exec": "allow"}}`,
		"memories/test.md":    "# test memory\nSome content here.",
		"chats/123/memory.md": "session notes",
		"budget.json":         `{"today": 1234}`,
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return files
}

// TestRC23W3SnapshotRestoreRoundTrip (F-A17-13): snapshot a data
// dir, delete the original entirely, restore, and verify every
// file's contents are byte-identical. Only scratch dirs are used;
// the real ~/.orion is never touched.
func TestRC23W3SnapshotRestoreRoundTrip(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "orion")
	backupDir := filepath.Join(t.TempDir(), "backups")
	want := rc23W3SeedDataDir(t, dataDir)

	snap, err := Snapshot(dataDir, backupDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snap, manifestFileName)); err != nil {
		t.Fatalf("snapshot has no manifest: %v", err)
	}
	snaps, err := List(backupDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Path != snap || snaps[0].Files != len(want) {
		t.Fatalf("List() = %+v, want 1 snapshot with %d files", snaps, len(want))
	}

	// Corrupt/delete the original: wipe the whole data dir.
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatalf("RemoveAll data dir: %v", err)
	}
	if err := Restore(snap, dataDir); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("restored %s: %v", rel, err)
			continue
		}
		if string(got) != content {
			t.Errorf("restored %s = %q, want %q", rel, got, content)
		}
	}
}

// TestRC23W3RestoreRefusesCorruptSnapshot (F-A17-13): tampering
// with one snapshot file after snapshotting makes Restore refuse
// the whole restore — and write nothing.
func TestRC23W3RestoreRefusesCorruptSnapshot(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "orion")
	backupDir := filepath.Join(t.TempDir(), "backups")
	rc23W3SeedDataDir(t, dataDir)
	snap, err := Snapshot(dataDir, backupDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Tamper with a snapshot file.
	if err := os.WriteFile(filepath.Join(snap, "config.json"), []byte(`{"tampered": true}`), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err := Restore(snap, target); err == nil {
		t.Fatal("Restore of a tampered snapshot: expected an error, got nil")
	} else if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Restore error = %q, want a corruption refusal", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("refused restore wrote %d entries, want none", len(entries))
	}
	// A snapshot with no manifest at all is refused too.
	emptySnap := filepath.Join(backupDir, "orion-nomanifest")
	if err := os.MkdirAll(emptySnap, 0o700); err != nil {
		t.Fatalf("mkdir empty snapshot: %v", err)
	}
	if err := Restore(emptySnap, target); err == nil {
		t.Error("Restore of a manifest-less snapshot: expected an error, got nil")
	}
}

// TestRC23W3PruneRotation (F-A17-13): Prune keeps the newest
// keep snapshots and deletes the rest. Snapshots are hand-made
// with distinct manifest timestamps so the test is deterministic
// without sleeps.
func TestRC23W3PruneRotation(t *testing.T) {
	backupDir := filepath.Join(t.TempDir(), "backups")
	for i := 0; i < 5; i++ {
		p := filepath.Join(backupDir, "orion-20260101-00000"+string(rune('0'+i)))
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatalf("mkdir snapshot %d: %v", i, err)
		}
		man := manifest{
			Version:   manifestVersion,
			CreatedAt: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
			Source:    "/nonexistent",
			Files:     []manifestFile{{Path: "config.json", Size: 1, SHA256: "x"}},
		}
		data, _ := json.Marshal(man)
		if err := os.WriteFile(filepath.Join(p, manifestFileName), data, 0o600); err != nil {
			t.Fatalf("write manifest %d: %v", i, err)
		}
	}
	removed, err := Prune(backupDir, 2)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("Prune(keep=2) removed %d, want 3", len(removed))
	}
	snaps, err := List(backupDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("List() after prune = %d snapshots, want 2", len(snaps))
	}
	// The two newest (i=4, i=3) survive.
	for _, s := range snaps {
		if !strings.HasSuffix(s.Path, "000004") && !strings.HasSuffix(s.Path, "000003") {
			t.Errorf("surviving snapshot %q is not one of the two newest", s.Path)
		}
	}
	// Prune with keep >= count removes nothing.
	if removed, err := Prune(backupDir, 7); err != nil || len(removed) != 0 {
		t.Errorf("Prune(keep=7) = %v, %v, want no removals", removed, err)
	}
}

// TestRC23W3SnapshotSkipsSymlink (F-A17-13): a symlink inside the
// data dir is never followed or copied, and restore does not
// recreate it.
func TestRC23W3SnapshotSkipsSymlink(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "orion")
	backupDir := filepath.Join(t.TempDir(), "backups")
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("do-not-copy"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	snap, err := Snapshot(dataDir, backupDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	manData, err := os.ReadFile(filepath.Join(snap, manifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(manData), `"path": "link"`) || strings.Contains(string(manData), "do-not-copy") {
		t.Errorf("manifest mentions the symlink or its target: %s", manData)
	}
	if _, err := os.Lstat(filepath.Join(snap, "link")); !os.IsNotExist(err) {
		t.Errorf("snapshot contains the symlink (err=%v), want it skipped", err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err := Restore(snap, target); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "link")); !os.IsNotExist(err) {
		t.Errorf("restored dir contains the symlink (err=%v), want it absent", err)
	}
}

// TestRC23W3SnapshotNamesUnique: rapid snapshots never collide.
func TestRC23W3SnapshotNamesUnique(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "orion")
	backupDir := filepath.Join(t.TempDir(), "backups")
	rc23W3SeedDataDir(t, dataDir)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		snap, err := Snapshot(dataDir, backupDir)
		if err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
		if seen[snap] {
			t.Fatalf("duplicate snapshot path %q", snap)
		}
		seen[snap] = true
	}
}
