// P2-O8 tests (rc8): the audit-log HMAC key must not live next to
// the log, and appends must be safe across processes (flock).
package security

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestP2AuditKeyRelocated: opening a log creates the key in the
// sibling <log-dir>-keys directory (0700 dir, 0600 file, 32 bytes),
// and NO key file exists in the log directory itself.
func TestP2AuditKeyRelocated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if _, err := NewAuditLog(path); err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	keyPath := auditKeyPath(dir)
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile key at %s: %v", keyPath, err)
	}
	if len(key) != auditKeyLen {
		t.Fatalf("key length = %d, want %d", len(key), auditKeyLen)
	}
	if st, err := os.Stat(keyPath); err != nil {
		t.Fatalf("Stat key: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 0600", st.Mode().Perm())
	}
	if st, err := os.Stat(auditKeyDir(dir)); err != nil {
		t.Fatalf("Stat key dir: %v", err)
	} else if st.Mode().Perm() != 0o700 {
		t.Errorf("key dir mode = %o, want 0700", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.key")); !os.IsNotExist(err) {
		t.Errorf("key file present in the log directory itself (err=%v); it must live in %s", err, auditKeyDir(dir))
	}
}

// TestP2AuditKeyLegacyMigration: a key left at the old location is
// moved — same bytes — to the new location on first open, the
// legacy file is removed, and the log keeps working under the
// migrated key.
func TestP2AuditKeyLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	legacyKey := make([]byte, auditKeyLen)
	if _, err := rand.Read(legacyKey); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(auditKeyLegacyPath(dir), legacyKey, 0o644); err != nil {
		t.Fatalf("WriteFile legacy key: %v", err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog with legacy key: %v", err)
	}
	migrated, err := os.ReadFile(auditKeyPath(dir))
	if err != nil {
		t.Fatalf("ReadFile migrated key: %v", err)
	}
	if !bytes.Equal(legacyKey, migrated) {
		t.Error("migrated key bytes differ from the legacy key (migration must preserve the key)")
	}
	if st, err := os.Stat(auditKeyPath(dir)); err != nil {
		t.Fatalf("Stat migrated key: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("migrated key mode = %o, want 0600", st.Mode().Perm())
	}
	if _, err := os.Stat(auditKeyLegacyPath(dir)); !os.IsNotExist(err) {
		t.Errorf("legacy key file still present after migration (err=%v)", err)
	}
	if _, err := log.Append("agent", "test", "after-migration"); err != nil {
		t.Fatalf("Append after migration: %v", err)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify after migration: %v", err)
	}
	// Reopening reuses the migrated key: the chain still verifies.
	reopened, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog (reopen): %v", err)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after reopen: %v", err)
	}
}

// TestP2AuditKeyMigrationPreservesChain: a log with v2 entries
// already chained under a legacy key must still verify after the
// migration moves the key — the entries are re-chained under the
// same key bytes, so nothing is orphaned.
func TestP2AuditKeyMigrationPreservesChain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	key := make([]byte, auditKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(auditKeyLegacyPath(dir), key, 0o600); err != nil {
		t.Fatalf("WriteFile legacy key: %v", err)
	}
	// Hand-build a 2-entry v2 chain under the legacy key, with a
	// signed head — exactly what an older build would have left.
	ts := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	e1 := Entry{Seq: 1, Timestamp: ts, Actor: "agent", Action: "a", Detail: "one", PrevHash: "", Version: auditVersionHMAC}
	e1.Hash = computeHashHMAC(key, e1.PrevHash, e1.Seq, e1.Timestamp, e1.Actor, e1.Action, e1.Detail)
	e2 := Entry{Seq: 2, Timestamp: ts, Actor: "agent", Action: "a", Detail: "two", PrevHash: e1.Hash, Version: auditVersionHMAC}
	e2.Hash = computeHashHMAC(key, e2.PrevHash, e2.Seq, e2.Timestamp, e2.Actor, e2.Action, e2.Detail)
	var buf bytes.Buffer
	for _, e := range []Entry{e1, e2} {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile log: %v", err)
	}
	if err := writeHead(path+".head", key, e2.Seq, e2.Hash); err != nil {
		t.Fatalf("writeHead: %v", err)
	}
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog on a pre-migration log: %v", err)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify after migration: %v", err)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want the 2 pre-migration entries", len(entries))
	}
	e3, err := log.Append("agent", "a", "three")
	if err != nil {
		t.Fatalf("Append after migration: %v", err)
	}
	if e3.Seq != 3 || e3.PrevHash != e2.Hash {
		t.Errorf("Append after migration = seq %d prev %.12s, want seq 3 chained on the pre-migration tip", e3.Seq, e3.PrevHash)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify after post-migration append: %v", err)
	}
}

// TestP2AuditKeyBothLocationsDiffer: keys at both locations with
// different bytes must fail the open rather than guessing which is
// authoritative.
func TestP2AuditKeyBothLocationsDiffer(t *testing.T) {
	dir := t.TempDir()
	keyA := make([]byte, auditKeyLen)
	keyB := make([]byte, auditKeyLen)
	if _, err := rand.Read(keyA); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if _, err := rand.Read(keyB); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.MkdirAll(auditKeyDir(dir), 0o700); err != nil {
		t.Fatalf("MkdirAll key dir: %v", err)
	}
	if err := os.WriteFile(auditKeyPath(dir), keyA, 0o600); err != nil {
		t.Fatalf("WriteFile new key: %v", err)
	}
	if err := os.WriteFile(auditKeyLegacyPath(dir), keyB, 0o600); err != nil {
		t.Fatalf("WriteFile legacy key: %v", err)
	}
	if _, err := NewAuditLog(filepath.Join(dir, "audit.jsonl")); err == nil {
		t.Error("NewAuditLog with different keys at both locations = nil, want error")
	}
}

// TestP2AuditKeyWrongLengthAtNewLocation: a short key at the NEW
// location is rejected, never silently replaced.
func TestP2AuditKeyWrongLengthAtNewLocation(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(auditKeyDir(dir), 0o700); err != nil {
		t.Fatalf("MkdirAll key dir: %v", err)
	}
	if err := os.WriteFile(auditKeyPath(dir), make([]byte, 16), 0o600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	if _, err := NewAuditLog(filepath.Join(dir, "audit.jsonl")); err == nil {
		t.Error("NewAuditLog with a 16-byte key at the new location = nil, want error")
	}
}

// TestP2AuditLockFileCreated: opening a log creates the flock file
// next to it (0600); its contents are irrelevant, only the flock
// on it matters.
func TestP2AuditLockFileCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if _, err := NewAuditLog(path); err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	st, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatalf("Stat lock file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("lock file mode = %o, want 0600", st.Mode().Perm())
	}
}

// TestP2AuditLogConcurrentAppendsTwoHandles: two handles on the same
// log (as an oriond daemon and a CLI App would hold) appending
// concurrently must produce one unbroken chain: sequence numbers
// 1..N exactly once, no gaps, no duplicates, Verify clean.
func TestP2AuditLogConcurrentAppendsTwoHandles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	h1, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog h1: %v", err)
	}
	h2, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog h2: %v", err)
	}
	const perGoroutine = 25
	const goroutines = 8
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			h := h1
			if n%2 == 1 {
				h = h2
			}
			for j := 0; j < perGoroutine; j++ {
				if _, err := h.Append("agent", "concurrent", fmt.Sprintf("g%d-%d", n, j)); err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent Append: %v", err)
	}
	if err := h1.Verify(); err != nil {
		t.Fatalf("Verify after concurrent appends: %v", err)
	}
	entries, err := h1.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	want := goroutines * perGoroutine
	if len(entries) != want {
		t.Fatalf("entries = %d, want %d", len(entries), want)
	}
	seqs := make([]int, 0, len(entries))
	for _, e := range entries {
		seqs = append(seqs, e.Seq)
	}
	sort.Ints(seqs)
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("sequence numbers not 1..%d exactly once: position %d has seq %d", want, i, s)
		}
	}
}

// TestP2AuditLogCrossProcessAppend: a real second PROCESS appending
// to the same log while this process appends must produce one
// unbroken chain. The helper is this same test binary, re-executed
// with -test.run selecting only this test and ORION_AUDIT_HELPER=1.
func TestP2AuditLogCrossProcessAppend(t *testing.T) {
	if os.Getenv("ORION_AUDIT_HELPER") == "1" {
		path := os.Getenv("ORION_AUDIT_HELPER_PATH")
		n := 0
		if _, err := fmt.Sscanf(os.Getenv("ORION_AUDIT_HELPER_N"), "%d", &n); err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "helper: bad ORION_AUDIT_HELPER_N\n")
			os.Exit(2)
		}
		log, err := NewAuditLog(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: NewAuditLog: %v\n", err)
			os.Exit(2)
		}
		for i := 0; i < n; i++ {
			if _, err := log.Append("helper", "x", fmt.Sprintf("helper-%d", i)); err != nil {
				fmt.Fprintf(os.Stderr, "helper: Append: %v\n", err)
				os.Exit(2)
			}
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	const helperN = 30
	const mainN = 30
	cmd := exec.Command(os.Args[0], "-test.run=TestP2AuditLogCrossProcessAppend", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"ORION_AUDIT_HELPER=1",
		"ORION_AUDIT_HELPER_PATH="+path,
		fmt.Sprintf("ORION_AUDIT_HELPER_N=%d", helperN),
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	// Append concurrently with the helper process.
	appendErr := make(chan error, 1)
	go func() {
		for i := 0; i < mainN; i++ {
			if _, err := log.Append("main", "x", fmt.Sprintf("main-%d", i)); err != nil {
				appendErr <- err
				return
			}
		}
		appendErr <- nil
	}()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process failed: %v (stderr: %s)", err, stderr.String())
	}
	if err := <-appendErr; err != nil {
		t.Fatalf("main Append during helper run: %v", err)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify after cross-process appends: %v", err)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != helperN+mainN {
		t.Fatalf("entries = %d, want %d (helper %d + main %d)", len(entries), helperN+mainN, helperN, mainN)
	}
	seqs := make([]int, 0, len(entries))
	for _, e := range entries {
		seqs = append(seqs, e.Seq)
	}
	sort.Ints(seqs)
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("sequence numbers not 1..%d exactly once: position %d has seq %d", helperN+mainN, i, s)
		}
	}
}
