package security

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- HMAC-signed audit chain (v2) tests ---
//
// Every test here uses its own temp directory (and therefore its own
// temp audit key, at <dir>-keys/audit.key); no test reads or writes
// the real ~/.orion key, and no test ever logs or prints key material.

// hmacLog is a small helper: open a fresh log in a temp dir and append
// n entries. It returns the log, the log path and the directory (which
// holds audit.jsonl.head; the key lives in the sibling <dir>-keys).
func hmacLog(t *testing.T, n int) (*AuditLog, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := log.Append("agent", "test-action", "entry"); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify before test step: %v", err)
	}
	return log, path, dir
}

// rewriteAuditEntries rewrites the log file with the given entries
// (one JSON line each, newline-terminated), leaving the key and head
// files untouched.
func rewriteAuditEntries(t *testing.T, path string, entries []Entry) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// readAuditHeadFile parses the head file next to the log.
func readAuditHeadFile(t *testing.T, logPath string) auditHead {
	t.Helper()
	data, err := os.ReadFile(logPath + ".head")
	if err != nil {
		t.Fatalf("ReadFile head: %v", err)
	}
	var head auditHead
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatalf("Unmarshal head: %v", err)
	}
	return head
}

// TestAuditKeyFileCreated0600AndReused: opening a log creates
// audit.key (0600, 32 bytes) in the key directory next to the log
// directory (<dir>-keys, P2-O8 — never in the log directory itself),
// and reopening reuses the same key — the chain appended under the
// first handle still verifies under the second.
func TestAuditKeyFileCreated0600AndReused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	keyPath := auditKeyPath(dir)
	key1, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile key: %v", err)
	}
	if len(key1) != auditKeyLen {
		t.Fatalf("key length = %d, want %d", len(key1), auditKeyLen)
	}
	// P2-O8: no key file may remain in the log directory itself.
	if _, err := os.Stat(filepath.Join(dir, "audit.key")); !os.IsNotExist(err) {
		t.Errorf("legacy key file still present in the log directory (err=%v)", err)
	}
	if st, err := os.Stat(keyPath); err != nil {
		t.Fatalf("Stat key: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 0600", st.Mode().Perm())
	}
	if _, err := log.Append("agent", "test", "first"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	reopened, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog (reopen): %v", err)
	}
	key2, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile key after reopen: %v", err)
	}
	if !bytes.Equal(key1, key2) {
		t.Error("key changed across reopen: want the same key reused")
	}
	if _, err := reopened.Append("agent", "test", "second"); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after reopen + append: %v", err)
	}
}

// TestAuditKeyWrongLengthRejected: a key file with the wrong length
// is an error, never silently replaced — replacing the key would
// orphan every v2 entry already in the log. The short key is seeded
// at the legacy location (migration input); a short key must be
// rejected there, not migrated.
func TestAuditKeyWrongLengthRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(auditKeyLegacyPath(dir), make([]byte, 16), 0o600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	if _, err := NewAuditLog(filepath.Join(dir, "audit.jsonl")); err == nil {
		t.Error("NewAuditLog with a 16-byte key = nil error, want error")
	}
}

// TestAuditKeyModeTightenedOnOpen: a pre-existing world-readable key
// file is tightened to 0600 on open, and its contents are kept. The
// key is seeded at the legacy location, so this also exercises
// migration: it must land at the new location, 0600, same bytes,
// with the legacy file removed.
func TestAuditKeyModeTightenedOnOpen(t *testing.T) {
	dir := t.TempDir()
	keyPath := auditKeyLegacyPath(dir)
	key := make([]byte, auditKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(keyPath, key, 0o644); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	if _, err := NewAuditLog(filepath.Join(dir, "audit.jsonl")); err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	// The migrated key lives at the new location, 0600, same bytes.
	newPath := auditKeyPath(dir)
	st, err := os.Stat(newPath)
	if err != nil {
		t.Fatalf("Stat migrated key: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("migrated key file mode = %o, want 0600 (tightened on open)", st.Mode().Perm())
	}
	after, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("ReadFile migrated key: %v", err)
	}
	if !bytes.Equal(key, after) {
		t.Error("key contents changed on open")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("legacy key file still present after migration (err=%v)", err)
	}
}

// TestAuditHeadFileCreated0600: after the first append the signed
// head checkpoint exists (0600), records the tip, and authenticates
// under the log's key.
func TestAuditHeadFileCreated0600(t *testing.T) {
	log, path, _ := hmacLog(t, 2)
	headPath := path + ".head"
	st, err := os.Stat(headPath)
	if err != nil {
		t.Fatalf("Stat head: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("head file mode = %o, want 0600", st.Mode().Perm())
	}
	head := readAuditHeadFile(t, path)
	if head.Version != auditVersionHMAC {
		t.Errorf("head Version = %d, want %d", head.Version, auditVersionHMAC)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	last := entries[len(entries)-1]
	if head.Seq != last.Seq || head.Hash != last.Hash {
		t.Errorf("head = {seq %d hash %.12s...}, want tip {seq %d hash %.12s...}", head.Seq, head.Hash, last.Seq, last.Hash)
	}
	if head.HMAC != headHMAC(log.key, head.Seq, head.Hash) {
		t.Error("head HMAC does not recompute under the log key")
	}
}

// TestAuditHMACTamperEveryField: changing ANY field of any entry —
// seq, timestamp, actor, action, detail, prev_hash, or version —
// without the key must be detected by Verify (and by reopening).
// The tampered field is rewritten with the stored hash left
// untouched, i.e. exactly what an attacker without the key can do.
func TestAuditHMACTamperEveryField(t *testing.T) {
	tamper := func(e *Entry, field string) {
		switch field {
		case "seq":
			e.Seq = 99
		case "timestamp":
			e.Timestamp = e.Timestamp.Add(time.Second)
		case "actor":
			e.Actor = "attacker"
		case "action":
			e.Action = "forged-action"
		case "detail":
			e.Detail = "forged detail"
		case "prev_hash":
			e.PrevHash = strings.Repeat("0", 64)
		case "version-downgrade":
			e.Version = auditVersionLegacy
		case "version-unknown":
			e.Version = 99
		default:
			t.Fatalf("unknown field %q", field)
		}
	}
	for _, field := range []string{"seq", "timestamp", "actor", "action", "detail", "prev_hash", "version-downgrade", "version-unknown"} {
		t.Run(field, func(t *testing.T) {
			log, path, _ := hmacLog(t, 3)
			entries, err := log.Entries()
			if err != nil {
				t.Fatalf("Entries: %v", err)
			}
			// Tamper the middle entry: its own hash check fails, and
			// the following entry's prev_hash linkage would fail too.
			tamper(&entries[1], field)
			rewriteAuditEntries(t, path, entries)
			if err := log.Verify(); err == nil {
				t.Errorf("Verify() after tampering %s = nil, want error", field)
			}
			if _, err := NewAuditLog(path); err == nil {
				t.Errorf("NewAuditLog() after tampering %s = nil, want error", field)
			}
		})
	}
}

// TestAuditHMACRecomputeWithoutKeyFails: an attacker who rewrites an
// entry AND recomputes the chain with the old unkeyed hash (the only
// recomputation possible without the key) must still be caught —
// the stored hash is HMAC, not SHA-256.
func TestAuditHMACRecomputeWithoutKeyFails(t *testing.T) {
	log, path, _ := hmacLog(t, 2)
	entries, err := log.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	// Attacker changes the detail and re-chains with the unkeyed hash.
	entries[1].Detail = "forged detail"
	entries[1].Hash = computeHash(entries[1].PrevHash, entries[1].Seq, entries[1].Timestamp, entries[1].Actor, entries[1].Action, entries[1].Detail)
	rewriteAuditEntries(t, path, entries)
	if err := log.Verify(); err == nil {
		t.Error("Verify() after unkeyed re-chain = nil, want error (v2 entries require the HMAC)")
	}
}

// TestAuditTruncationDetected: cleanly removing the last complete
// line(s) — the file still ends with a newline, so this is NOT a
// torn tail — must be detected by Verify and by reopening, because
// the signed head still records the missing tip.
func TestAuditTruncationDetected(t *testing.T) {
	for _, drop := range []int{1, 2} {
		t.Run(fmt.Sprintf("drop-%d", drop), func(t *testing.T) {
			log, path, _ := hmacLog(t, 3)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
			kept := bytes.Join(lines[:len(lines)-drop], []byte("\n"))
			kept = append(kept, '\n')
			if err := os.WriteFile(path, kept, 0o600); err != nil {
				t.Fatalf("WriteFile truncated: %v", err)
			}
			err = log.Verify()
			if err == nil {
				t.Fatalf("Verify() after dropping %d line(s) = nil, want truncation error", drop)
			}
			if !strings.Contains(err.Error(), "truncated") {
				t.Errorf("Verify() error = %v, want it to mention truncation", err)
			}
			if _, err := NewAuditLog(path); err == nil {
				t.Errorf("NewAuditLog() after dropping %d line(s) = nil, want error", drop)
			}
		})
	}
}

// TestAuditTruncationUndetectableWithoutHead documents the boundary
// of the checkpoint: if the head file is deleted together with the
// truncated tail, there is no checkpoint left to contradict the
// shorter log, and the remaining chain verifies as a shorter valid
// log. The head must therefore be protected like the log itself
// (0600 in the 0700 log directory); this test pins the limitation
// so a future change cannot silently assume otherwise.
func TestAuditTruncationUndetectableWithoutHead(t *testing.T) {
	log, path, dir := hmacLog(t, 3)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	kept := append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n')
	if err := os.WriteFile(path, kept, 0o600); err != nil {
		t.Fatalf("WriteFile truncated: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "audit.jsonl.head")); err != nil {
		t.Fatalf("Remove head: %v", err)
	}
	// No head, no contradiction: the 2-entry prefix is a valid chain.
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify() with head deleted = %v, want nil (documented limitation)", err)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want the 2 remaining", len(entries))
	}
}

// TestAuditHeadAbsentDegradesToChainOnly: deleting just the head file
// (log intact) still verifies — the chain check alone is the
// fallback, and a missing head is not an error.
func TestAuditHeadAbsentDegradesToChainOnly(t *testing.T) {
	log, path, dir := hmacLog(t, 2)
	if err := os.Remove(filepath.Join(dir, "audit.jsonl.head")); err != nil {
		t.Fatalf("Remove head: %v", err)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify() with head deleted and log intact = %v, want nil", err)
	}
	// Reopening re-creates the head at the verified tip.
	if _, err := NewAuditLog(path); err != nil {
		t.Fatalf("NewAuditLog after head deletion: %v", err)
	}
	if _, err := os.Stat(path + ".head"); err != nil {
		t.Fatalf("head not re-created on open: %v", err)
	}
}

// TestAuditWrongKeyDetected: replacing the key file with a different
// (freshly generated, temp) key must make the v2 chain fail
// verification — both the entry HMACs and the head HMAC are keyed.
// Restoring the original key must make the same log verify again.
func TestAuditWrongKeyDetected(t *testing.T) {
	_, path, dir := hmacLog(t, 2)
	keyPath := auditKeyPath(dir)
	origKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile key: %v", err)
	}
	wrongKey := make([]byte, auditKeyLen)
	if _, err := rand.Read(wrongKey); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(keyPath, wrongKey, 0o600); err != nil {
		t.Fatalf("WriteFile wrong key: %v", err)
	}
	if _, err := NewAuditLog(path); err == nil {
		t.Error("NewAuditLog with a different key = nil error, want error")
	}
	// The original key still verifies the untouched log.
	if err := os.WriteFile(keyPath, origKey, 0o600); err != nil {
		t.Fatalf("WriteFile original key: %v", err)
	}
	reopened, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog after restoring the key: %v", err)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after restoring the key: %v", err)
	}
}

// TestAuditHeadTampered: forging the head checkpoint (changing its
// recorded hash without re-signing it) must be detected — the head
// HMAC is keyed, so the forgery cannot be re-signed without the key.
func TestAuditHeadTampered(t *testing.T) {
	log, path, _ := hmacLog(t, 2)
	head := readAuditHeadFile(t, path)
	head.Hash = strings.Repeat("0", 64) // forged tip hash, HMAC untouched
	data, err := json.Marshal(head)
	if err != nil {
		t.Fatalf("Marshal head: %v", err)
	}
	if err := os.WriteFile(path+".head", append(data, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile head: %v", err)
	}
	if err := log.Verify(); err == nil {
		t.Error("Verify() after forging the head = nil, want error")
	}
	if _, err := NewAuditLog(path); err == nil {
		t.Error("NewAuditLog() after forging the head = nil, want error")
	}
}

// TestAuditLegacyLogGainsHeadOnOpen: a v1 (unkeyed) log opened by a
// v2 build verifies as legacy, and the open writes a signed head at
// the verified tip — so from then on even the legacy entries are
// protected against clean tail truncation.
func TestAuditLegacyLogGainsHeadOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	ts := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	e := Entry{
		Seq: 1, Timestamp: ts, Actor: "user", Action: "test", Detail: "legacy entry",
		PrevHash: "",
	}
	e.Hash = computeHashV1(e.PrevHash, e.Seq, e.Timestamp, e.Actor, e.Action, e.Detail)
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog on a v1 log: %v", err)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify on a v1 log: %v", err)
	}
	head := readAuditHeadFile(t, path)
	if head.Seq != 1 || head.Hash != e.Hash {
		t.Errorf("head after opening a v1 log = {seq %d}, want {seq 1 hash of the legacy entry}", head.Seq)
	}
	// Now the legacy tail is protected: deleting the only line is
	// caught by the head.
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("WriteFile emptied log: %v", err)
	}
	if err := log.Verify(); err == nil {
		t.Error("Verify() after truncating a head-protected v1 log = nil, want truncation error")
	}
}

// TestAuditMixedV1V2ChainVerifies: a v1 entry followed by v2 appends
// verifies as a mixed chain, with the new entries marked version 2.
func TestAuditMixedV1V2ChainVerifies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	ts := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	e := Entry{
		Seq: 1, Timestamp: ts, Actor: "user", Action: "test", Detail: "old-format entry",
		PrevHash: "",
	}
	e.Hash = computeHashV1(e.PrevHash, e.Seq, e.Timestamp, e.Actor, e.Action, e.Detail)
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog on a v1 log: %v", err)
	}
	appended, err := log.Append("user", "test2", "new-format entry")
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if appended.Version != auditVersionHMAC {
		t.Errorf("appended Version = %d, want %d", appended.Version, auditVersionHMAC)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify on a mixed v1+v2 log: %v", err)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Version != auditVersionLegacy {
		t.Errorf("entries[0].Version = %d, want %d (legacy)", entries[0].Version, auditVersionLegacy)
	}
	if entries[1].Version != auditVersionHMAC {
		t.Errorf("entries[1].Version = %d, want %d (HMAC)", entries[1].Version, auditVersionHMAC)
	}
}

// TestAuditTornTailStillRecoveredV2: the v2 torn-tail behavior is
// unchanged — an unterminated fragment (crash mid-Append, no
// trailing newline) is dropped on open, while the signed head still
// covers the last complete entry. This is the boundary vs
// TestAuditTruncationDetected: a fragment with no newline is a torn
// write; a complete line cleanly removed is truncation.
func TestAuditTornTailStillRecoveredV2(t *testing.T) {
	_, path, _ := hmacLog(t, 3)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	torn := append(bytes.Clone(raw),
		[]byte(`{"seq":4,"timestamp":"2026-11-13T02:00:00Z","actor":"age`)...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatalf("WriteFile torn log: %v", err)
	}
	reopened, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog on torn v2 log: %v (want recovery, not an error)", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after recovery: %v", err)
	}
	if !bytes.Equal(after, raw) {
		t.Fatal("torn tail was not truncated: file differs from the pre-crash bytes")
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after recovery: %v", err)
	}
	entries, err := reopened.Entries()
	if err != nil {
		t.Fatalf("Entries after recovery: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries after recovery = %d, want 3", len(entries))
	}
}
