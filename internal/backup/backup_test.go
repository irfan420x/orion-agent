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

// TestRC24SnapshotManifestNotesLiveSnapshot (F-A18-8, rc24): every
// manifest Snapshot writes must say honestly that it is a live,
// non-quiesced snapshot — quiesced=false plus a plain-language
// note — so a restore operator knows the snapshot may mix moments.
func TestRC24SnapshotManifestNotesLiveSnapshot(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "orion")
	backupDir := filepath.Join(t.TempDir(), "backups")
	rc23W3SeedDataDir(t, dataDir)
	snap, err := Snapshot(dataDir, backupDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	manData, err := os.ReadFile(filepath.Join(snap, manifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var man manifest
	if err := json.Unmarshal(manData, &man); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if man.Quiesced {
		t.Errorf("manifest quiesced = true, want false (Snapshot never quiesces the daemon)")
	}
	if !strings.Contains(man.Consistency, "live snapshot") || !strings.Contains(man.Consistency, "mix moments") {
		t.Errorf("manifest consistency = %q, want the live-snapshot warning", man.Consistency)
	}
	// The raw JSON must carry the fields too — the operator reads
	// the file, not the Go struct.
	if !strings.Contains(string(manData), `"quiesced": false`) {
		t.Errorf("manifest JSON lacks \"quiesced\": false: %s", manData)
	}
	if !strings.Contains(string(manData), `"consistency":`) {
		t.Errorf("manifest JSON lacks a consistency field: %s", manData)
	}
}

// TestRC24RestoreAcceptsPreRC24Manifest (F-A18-8, rc24): a manifest
// written before the quiesced/consistency fields existed (rc23 and
// older) must still restore — the new fields are additive, and a
// missing quiesced flag decodes as false, which is correct for
// those live snapshots too.
func TestRC24RestoreAcceptsPreRC24Manifest(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "orion")
	backupDir := filepath.Join(t.TempDir(), "backups")
	want := rc23W3SeedDataDir(t, dataDir)
	snap, err := Snapshot(dataDir, backupDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Strip the rc24 fields back to the rc23 shape: version,
	// created_at, source, files only.
	manData, err := os.ReadFile(filepath.Join(snap, manifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(manData, &raw); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	delete(raw, "quiesced")
	delete(raw, "consistency")
	oldData, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encode old manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snap, manifestFileName), oldData, 0o600); err != nil {
		t.Fatalf("write old manifest: %v", err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err := Restore(snap, target); err != nil {
		t.Fatalf("Restore of a pre-rc24 manifest: %v", err)
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("restored %s: %v", rel, err)
			continue
		}
		if string(got) != content {
			t.Errorf("restored %s = %q, want %q", rel, got, content)
		}
	}
}

// TestRC24RestoreSkipsInstanceLock (F-A18-2): the instance-lock
// file is a runtime artifact, not data — Restore must never
// verify or replace the live oriond.lock, so the restoring
// process's flock stays valid through the restore and no second
// daemon can grab a fresh lock mid-restore.
func TestRC24RestoreSkipsInstanceLock(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "orion")
	backupDir := filepath.Join(t.TempDir(), "backups")
	want := rc23W3SeedDataDir(t, dataDir)
	// The snapshot's lock file carries marker content so a
	// replacement would be visible.
	if err := os.WriteFile(filepath.Join(dataDir, "oriond.lock"), []byte("snapshot-lock"), 0o600); err != nil {
		t.Fatalf("write snapshot lock: %v", err)
	}
	snap, err := Snapshot(dataDir, backupDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// The manifest must list the lock file — otherwise this test
	// would pass vacuously without exercising the skip.
	manData, err := os.ReadFile(filepath.Join(snap, manifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(manData), "oriond.lock") {
		t.Fatalf("manifest does not list oriond.lock — the skip is untested:\n%s", manData)
	}

	// The live data dir has its own lock file (held by the
	// restoring process in production); Restore must leave it
	// exactly as it is.
	target := filepath.Join(t.TempDir(), "restored")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	liveLock := filepath.Join(target, "oriond.lock")
	if err := os.WriteFile(liveLock, []byte("live-lock"), 0o600); err != nil {
		t.Fatalf("write live lock: %v", err)
	}
	liveBefore, err := os.Stat(liveLock)
	if err != nil {
		t.Fatalf("stat live lock: %v", err)
	}
	if err := Restore(snap, target); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(liveLock)
	if err != nil {
		t.Fatalf("read live lock: %v", err)
	}
	if string(got) != "live-lock" {
		t.Errorf("live oriond.lock = %q, want %q (Restore must not replace it)", got, "live-lock")
	}
	liveAfter, err := os.Stat(liveLock)
	if err != nil {
		t.Fatalf("stat live lock after: %v", err)
	}
	if !os.SameFile(liveBefore, liveAfter) {
		t.Errorf("live oriond.lock was replaced (different inode) — the flock would go stale")
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("restored %s: %v", rel, err)
			continue
		}
		if string(got) != content {
			t.Errorf("restored %s = %q, want %q", rel, got, content)
		}
	}

	// A tampered snapshot lock file must not block the restore
	// either — it is never verified.
	if err := os.WriteFile(filepath.Join(snap, "oriond.lock"), []byte("tampered-lock"), 0o600); err != nil {
		t.Fatalf("tamper snapshot lock: %v", err)
	}
	target2 := filepath.Join(t.TempDir(), "restored2")
	if err := Restore(snap, target2); err != nil {
		t.Fatalf("Restore with tampered snapshot lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target2, "config.json")); err != nil {
		t.Errorf("restored config.json missing after tampered-lock restore: %v", err)
	}
}
