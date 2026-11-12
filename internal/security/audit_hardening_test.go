package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- Hardening tests (audit P1-12) ---

// TestAuditV1LogStillVerifies: a log written with the old (pre-
// hardening) hash format must still verify, and new entries appended
// to it (new format) must keep the mixed chain valid.
func TestAuditV1LogStillVerifies(t *testing.T) {
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
	al, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog on a v1 log: %v", err)
	}
	if err := al.Verify(); err != nil {
		t.Fatalf("Verify on a v1 log: %v", err)
	}
	if _, err := al.Append("user", "test2", "new-format entry"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := al.Verify(); err != nil {
		t.Fatalf("Verify on a mixed v1+v2 log: %v", err)
	}
	entries, err := al.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

// TestComputeHashFieldBoundaries: moving characters between fields
// must change the hash. The old plain-concatenation hash could not
// tell ("ab","c") from ("a","bc"); the length-prefixed hash can.
func TestComputeHashFieldBoundaries(t *testing.T) {
	ts := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	h1 := computeHash("", 1, ts, "ab", "c", "detail")
	h2 := computeHash("", 1, ts, "a", "bc", "detail")
	if h1 == h2 {
		t.Fatal("hashes are identical for (actor=ab, action=c) vs (actor=a, action=bc): field boundaries are ambiguous")
	}
	// And the old format really did collide, proving the test is
	// sensitive to the bug it guards.
	if computeHashV1("", 1, ts, "ab", "c", "detail") == computeHashV1("", 1, ts, "a", "bc", "detail") {
		// Expected: v1 collides. (If this ever fails, v1 changed.)
	} else {
		t.Fatal("v1 hashes differ unexpectedly — the collision the test guards against is gone")
	}
}
