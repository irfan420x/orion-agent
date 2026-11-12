// Package security — append-only, hash-chained audit log.
//
// Every entry is one JSON line in the log file. Each entry's Hash
// chains it to the previous one via PrevHash, so any modification of
// any line — or any reordering, deletion or insertion — breaks the
// chain and is caught by Verify.
//
// Versioning (see Entry.Version):
//
//   - v0 (legacy): the chain hash is unkeyed SHA-256 over the entry
//     fields (computeHash, length-prefixed; before that computeHashV1,
//     plain concatenation). A v0 chain verifies as legacy, but anyone
//     with write access to the log can recompute it after tampering,
//     and clean truncation of the tail is undetectable without a
//     checkpoint. v0 is accepted on read, never produced on write.
//   - v2 (current): the chain hash is HMAC-SHA256 keyed by a 32-byte
//     key stored in <log-dir>/audit.key (0600, generated with
//     crypto/rand on first open; in production the log lives in
//     ~/.orion, so the key is ~/.orion/audit.key). Recomputing the
//     chain after tampering requires the key, and verification with a
//     different key fails. New entries are always v2.
//
// Truncation detection: after every successful Append the log also
// updates a signed head checkpoint (<log>.head) recording the tip
// sequence number and hash, authenticated with the same key. Verify
// rejects a log whose last entry is behind the head (cleanly
// truncated tail) or whose entry at the head's sequence no longer
// matches the head. A log with no head file (a legacy log that has
// never been opened by a v2 build, or a log with no entries yet)
// verifies the chain only and cannot detect tail truncation —
// opening such a log with NewAuditLog writes a head at the verified
// tip, so the checkpoint is established from the first v2 open.
// Deleting the head file together with truncating the log defeats
// the checkpoint: the head is a checkpoint, not a separate trust
// root, and it must be protected like the log itself (0600, in the
// 0700 log directory).
package security

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// auditVersionLegacy (0) is an entry whose Hash is unkeyed SHA-256
// (computeHash or the older computeHashV1). The field is absent from
// the JSON of entries written before versioning, so 0 is the legacy
// default. auditVersionHMAC (2) is an entry whose Hash is
// HMAC-SHA256 under the audit key. There is no version 1: the
// length-prefixed unkeyed hash was the second hash format, and
// version 2 marks the keyed one.
const (
	auditVersionLegacy = 0
	auditVersionHMAC   = 2
)

// Entry is one audit-log record. Hash chains this entry to the previous
// one via PrevHash; the first (genesis) entry has an empty PrevHash.
// Version selects the hash scheme: 0 (absent) is the legacy unkeyed
// SHA-256 chain, 2 is the HMAC-SHA256 chain. Any other value is
// rejected by Verify.
type Entry struct {
	Seq       int       `json:"seq"`
	Timestamp time.Time `json:"timestamp"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	PrevHash  string    `json:"prev_hash"`
	Hash      string    `json:"hash"`
	Version   int       `json:"version,omitempty"`
}

// computeHash returns hex(sha256(...)) over the entry fields. Every
// field is length-prefixed, so field boundaries are unambiguous: moving
// characters between fields changes the hash. It is the single
// definition of the chain hash; Append and Verify both use it.
func computeHash(prevHash string, seq int, ts time.Time, actor, action, detail string) string {
	h := sha256.New()
	writeField := func(s string) {
		fmt.Fprintf(h, "%d:%s;", len(s), s)
	}
	writeField(prevHash)
	writeField(strconv.Itoa(seq))
	writeField(ts.Format(time.RFC3339Nano))
	writeField(actor)
	writeField(action)
	writeField(detail)
	return hex.EncodeToString(h.Sum(nil))
}

// computeHashV1 is the pre-hardening hash (plain concatenation, no
// length prefixes). It is kept only so logs written before the fix
// still verify; new entries always use computeHash. A v1 hash is
// accepted on read, never produced on write.
func computeHashV1(prevHash string, seq int, ts time.Time, actor, action, detail string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s%d%s%s%s%s",
		prevHash,
		seq,
		ts.Format(time.RFC3339Nano),
		actor,
		action,
		detail,
	)
	return hex.EncodeToString(h.Sum(nil))
}

// computeHashHMAC returns hex(HMAC-SHA256(key, ...)) over the entry
// fields, length-prefixed exactly like computeHash. The domain label
// "orion-audit-v2" and the version are part of the MAC input, so a v2
// hash can never validate as a legacy entry and flipping Version on a
// stored entry breaks its hash. It is the only hash produced on
// write; computeHash and computeHashV1 survive solely as legacy
// verifiers. The key is never logged or printed.
func computeHashHMAC(key []byte, prevHash string, seq int, ts time.Time, actor, action, detail string) string {
	mac := hmac.New(sha256.New, key)
	writeField := func(s string) {
		fmt.Fprintf(mac, "%d:%s;", len(s), s)
	}
	writeField("orion-audit-v2")
	writeField(strconv.Itoa(auditVersionHMAC))
	writeField(prevHash)
	writeField(strconv.Itoa(seq))
	writeField(ts.Format(time.RFC3339Nano))
	writeField(actor)
	writeField(action)
	writeField(detail)
	return hex.EncodeToString(mac.Sum(nil))
}

// AuditLog is an append-only JSON-lines audit log with a hash chain.
// New entries are chained with HMAC-SHA256 under a persistent key
// (see audit_key.go) and a signed head checkpoint records the chain
// tip after every append, so Verify also detects clean truncation of
// the tail. It is safe for concurrent use: Append takes the write lock,
// while Verify and Entries take the read lock, so reads never block
// appends (and vice versa only for the duration of an in-flight read).
type AuditLog struct {
	mu       sync.RWMutex
	path     string
	headPath string
	key      []byte
	lastSeq  int
	lastHash string
}

// NewAuditLog opens (or creates) the audit log at path. The parent
// directory is created with mode 0700 if missing and the log file with
// mode 0600. Modes are enforced on every open, not only on creation:
// a pre-existing world-readable audit.jsonl is tightened to 0600 (and
// its parent directory to 0700). The HMAC key is loaded or created in
// the same directory (audit.key, 0600; see loadOrCreateAuditKey). An
// existing log is fully loaded and its hash chain verified; a corrupt
// chain, a malformed line, a wrong key, a tampered head, or a log
// truncated behind its signed head is an error and the log is not
// opened. A torn tail — a partially written last line left by a crash
// mid-Append, with no trailing newline — is truncated on open so the
// log recovers instead of refusing to open; see recoverTornTail.
// On a successful open the signed head is advanced to the verified
// tip, so a legacy log (or a log whose last append crashed between
// the log fsync and the head update) gains a truncation checkpoint.
func NewAuditLog(path string) (*AuditLog, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("security: audit log path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("security: create audit log dir %s: %w", dir, err)
	}
	// Enforce the directory mode on open, not only when created.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("security: chmod audit log dir %s: %w", dir, err)
	}
	// Load (or generate) the HMAC key before touching the log: a v2
	// chain cannot be verified without it, and a fresh key must never
	// be generated after the log is read.
	key, err := loadOrCreateAuditKey(dir)
	if err != nil {
		return nil, err
	}
	// Ensure the file exists with private permissions.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("security: create audit log %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("security: close audit log %s: %w", path, err)
	}
	// Enforce the file mode on open, not only when created: a
	// pre-existing world-readable log is tightened.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("security: chmod audit log %s: %w", path, err)
	}
	al := &AuditLog{path: path, headPath: path + ".head", key: key}
	// Recover a torn tail from a crash mid-Append before verifying.
	if err := recoverTornTail(path); err != nil {
		return nil, err
	}
	entries, err := al.readAndVerify(false)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		al.lastSeq = last.Seq
		al.lastHash = last.Hash
		// Advance the signed head to the verified tip: this closes
		// the window where a log entry exists but no checkpoint
		// covers it (crash between log fsync and head update, or a
		// legacy log opened for the first time by a v2 build).
		if err := writeHead(al.headPath, key, last.Seq, last.Hash); err != nil {
			return nil, err
		}
	}
	return al, nil
}

// recoverTornTail truncates a partially written last line. Append
// writes one JSON line plus "\n" in a single Write, so a crash
// mid-write leaves a trailing fragment with no newline. A fragment
// that still parses as a complete entry (only the newline was lost)
// is kept — readAndVerify will check its hash and sequence — and any
// other unterminated fragment is dropped by truncating the file to
// the end of the last complete line. A malformed line that DOES end
// with a newline is not a torn write: it is corruption or tampering
// and is left untouched for readAndVerify to reject.
func recoverTornTail(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("security: read audit log %s: %w", path, err)
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}
	idx := bytes.LastIndexByte(data, '\n')
	var e Entry
	if json.Unmarshal(data[idx+1:], &e) == nil {
		// Complete entry, only the newline was lost.
		return nil
	}
	if err := os.Truncate(path, int64(idx+1)); err != nil {
		return fmt.Errorf("security: truncate torn audit log tail %s: %w", path, err)
	}
	return nil
}

// auditHead is the signed checkpoint of the chain tip: the sequence
// number and hash of the last entry known to be in the log, with an
// HMAC over both so the checkpoint itself cannot be forged without
// the audit key. It lives in <log>.head and is rewritten atomically
// after every successful Append (and advanced to the verified tip on
// open). It is what makes clean truncation of the tail detectable:
// a log whose last entry is behind the head has lost entries.
type auditHead struct {
	Version int    `json:"version"`
	Seq     int    `json:"seq"`
	Hash    string `json:"hash"`
	HMAC    string `json:"hmac"`
}

// headHMAC authenticates a head checkpoint: HMAC-SHA256 over the
// domain label, the sequence number and the tip hash, all
// length-prefixed so field boundaries are unambiguous.
func headHMAC(key []byte, seq int, hash string) string {
	mac := hmac.New(sha256.New, key)
	writeField := func(s string) {
		fmt.Fprintf(mac, "%d:%s;", len(s), s)
	}
	writeField("orion-audit-head-v1")
	writeField(strconv.Itoa(seq))
	writeField(hash)
	return hex.EncodeToString(mac.Sum(nil))
}

// writeHead atomically replaces the head file: write to a temp file in
// the same directory, fsync, chmod 0600, rename over the old head,
// fsync the directory. A crash mid-update leaves either the old or
// the new head, never a half-written one.
func writeHead(path string, key []byte, seq int, hash string) error {
	head := auditHead{Version: auditVersionHMAC, Seq: seq, Hash: hash, HMAC: headHMAC(key, seq, hash)}
	data, err := json.Marshal(head)
	if err != nil {
		return fmt.Errorf("security: marshal audit head: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".audit-head-*")
	if err != nil {
		return fmt.Errorf("security: create temp audit head in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp file on any failure path; after
	// a successful rename the name no longer exists and Remove is a
	// harmless no-op.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("security: write temp audit head %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("security: fsync temp audit head %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("security: close temp audit head %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("security: chmod temp audit head %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("security: rename audit head %s: %w", path, err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("security: fsync audit head dir %s: %w", dir, err)
	}
	return nil
}

// readHead reads and authenticates the head file. It reports ok=false
// when no head file exists (a legacy log, or a log with no entries
// yet): truncation detection needs a head, so such logs verify the
// chain only. A present-but-corrupt head is an error, never ignored:
// silently dropping the checkpoint would hide exactly the truncation
// it exists to detect.
func readHead(path string, key []byte) (head auditHead, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return auditHead{}, false, nil
		}
		return auditHead{}, false, fmt.Errorf("security: read audit head %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return auditHead{}, false, fmt.Errorf("security: audit head %s is malformed: %w", path, err)
	}
	if head.Version != auditVersionHMAC {
		return auditHead{}, false, fmt.Errorf("security: audit head %s has unknown version %d", path, head.Version)
	}
	if head.Seq < 1 || head.Hash == "" {
		return auditHead{}, false, fmt.Errorf("security: audit head %s is incomplete (seq=%d)", path, head.Seq)
	}
	want, err := hex.DecodeString(head.HMAC)
	if err != nil {
		return auditHead{}, false, fmt.Errorf("security: audit head %s has a malformed hmac", path)
	}
	got, err := hex.DecodeString(headHMAC(key, head.Seq, head.Hash))
	if err != nil {
		return auditHead{}, false, fmt.Errorf("security: audit head %s: %v", path, err)
	}
	if !hmac.Equal(got, want) {
		return auditHead{}, false, fmt.Errorf("security: audit head %s failed authentication (head tampered or wrong key)", path)
	}
	return head, true, nil
}

// readAndVerify reads every entry from the log file and verifies the
// hash chain: each entry's PrevHash must equal the previous entry's
// Hash (empty for the genesis entry), sequence numbers must increase
// by exactly one from 1, and each Hash must recompute from the
// entry's own fields under the scheme its Version selects (see
// verifyEntry). It returns the entries in file order. Finally, when
// a signed head checkpoint exists, the log must contain at least the
// head's entry and that entry's hash must match the head — otherwise
// the tail was cleanly truncated or the entry at the head's sequence
// was rewritten.
//
// When tolerateTornTail is true (Verify/Entries, which can run
// concurrently with an Append), an unterminated last line that fails
// to parse or verify is treated as an in-flight partial write and
// ignored instead of failing the whole read; the log is reported as
// ending at the last complete entry. A malformed line that ends with
// a newline is never tolerated: that is corruption or tampering.
// NewAuditLog passes false — it truncates a torn tail first and then
// verifies strictly.
//
// Torn-tail boundary: tolerance applies only to an unterminated
// fragment (a crash mid-write, no trailing newline). A complete line
// that was cleanly removed — the file still ends with a newline, or
// the whole last line is gone — is NOT a torn tail: with a head
// checkpoint it is detected as truncation (see above); without a
// head (legacy log never opened by a v2 build) it is undetectable,
// which is exactly the weakness the v2 head exists to close.
func (a *AuditLog) readAndVerify(tolerateTornTail bool) ([]Entry, error) {
	path := a.path
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("security: open audit log %s: %w", path, err)
	}

	var entries []Entry
	prevHash := ""
	expectedSeq := 1
	unterminated := len(data) > 0 && data[len(data)-1] != '\n'
	lines := bytes.Split(data, []byte("\n"))
	for i, raw := range lines {
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		lineNo := i + 1
		isLast := i == len(lines)-1
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			if tolerateTornTail && unterminated && isLast {
				break
			}
			return nil, fmt.Errorf("security: audit log %s line %d: malformed entry: %w", path, lineNo, err)
		}
		if e.Seq != expectedSeq {
			if tolerateTornTail && unterminated && isLast {
				break
			}
			return nil, fmt.Errorf("security: audit log %s line %d: expected seq %d, got %d", path, lineNo, expectedSeq, e.Seq)
		}
		if e.PrevHash != prevHash {
			if tolerateTornTail && unterminated && isLast {
				break
			}
			return nil, fmt.Errorf("security: audit log %s line %d: prev_hash mismatch (chain broken)", path, lineNo)
		}
		if err := a.verifyEntry(e); err != nil {
			if tolerateTornTail && unterminated && isLast {
				break
			}
			return nil, fmt.Errorf("security: audit log %s line %d: %w", path, lineNo, err)
		}
		entries = append(entries, e)
		prevHash = e.Hash
		expectedSeq++
	}
	// Head checkpoint: the log must reach at least the head's entry,
	// and that entry must still hash as the head records. A shorter
	// log lost its tail; a mismatching entry was rewritten.
	head, ok, err := readHead(a.headPath, a.key)
	if err != nil {
		return nil, fmt.Errorf("security: audit log %s: %w", path, err)
	}
	if ok {
		if head.Seq > len(entries) {
			return nil, fmt.Errorf("security: audit log %s: truncated: signed head records seq %d but the log ends at seq %d", path, head.Seq, len(entries))
		}
		if entries[head.Seq-1].Hash != head.Hash {
			return nil, fmt.Errorf("security: audit log %s: head checkpoint mismatch at seq %d (entry rewritten or log replaced)", path, head.Seq)
		}
	}
	return entries, nil
}

// verifyEntry checks one entry's hash under the scheme its Version
// selects, covering every field: any change to seq, timestamp,
// actor, action, detail or prev_hash changes the recomputed hash.
//   - Version 0 (legacy): the unkeyed length-prefixed hash, or the
//     older plain-concatenation hash. Accepted on read, never
//     produced on write; it cannot detect recomputation by an
//     attacker who rewrites the chain, which is why v2 exists.
//   - Version 2: HMAC-SHA256 under the audit key. Recomputation
//     without the key fails, and verification with the wrong key
//     fails (constant-time comparison).
//   - Any other version is rejected outright.
func (a *AuditLog) verifyEntry(e Entry) error {
	switch e.Version {
	case auditVersionLegacy:
		want := computeHash(e.PrevHash, e.Seq, e.Timestamp, e.Actor, e.Action, e.Detail)
		if e.Hash != want && e.Hash != computeHashV1(e.PrevHash, e.Seq, e.Timestamp, e.Actor, e.Action, e.Detail) {
			return errors.New("hash mismatch (entry tampered)")
		}
		return nil
	case auditVersionHMAC:
		if a.key == nil {
			return errors.New("v2 entry but no HMAC key loaded")
		}
		want, err := hex.DecodeString(computeHashHMAC(a.key, e.PrevHash, e.Seq, e.Timestamp, e.Actor, e.Action, e.Detail))
		if err != nil {
			return fmt.Errorf("recomputed v2 hash is malformed: %w", err)
		}
		got, err := hex.DecodeString(e.Hash)
		if err != nil {
			return errors.New("hash mismatch (entry hash is malformed hex)")
		}
		if !hmac.Equal(got, want) {
			return errors.New("hash mismatch (entry tampered or wrong key)")
		}
		return nil
	default:
		return fmt.Errorf("unknown audit entry version %d", e.Version)
	}
}

// Append adds one v2 (HMAC-signed) entry to the log, fsyncing the
// write so it survives a crash, then atomically advances the signed
// head checkpoint to the new tip so truncation of this entry is
// detectable. It is mutex-protected: concurrent appends keep a single
// chain. If the head update fails, Append returns an error even
// though the entry itself is already durable in the log — the next
// open re-advances the head from the verified tip, but until then
// the checkpoint is stale and truncation of the new entry would not
// be detected, so the failure must not be silent.
func (a *AuditLog) Append(actor, action, detail string) (Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	e := Entry{
		Seq:       a.lastSeq + 1,
		Timestamp: time.Now().UTC(),
		Actor:     actor,
		Action:    action,
		Detail:    detail,
		PrevHash:  a.lastHash,
		Version:   auditVersionHMAC,
	}
	e.Hash = computeHashHMAC(a.key, e.PrevHash, e.Seq, e.Timestamp, e.Actor, e.Action, e.Detail)

	line, err := json.Marshal(e)
	if err != nil {
		return Entry{}, fmt.Errorf("security: marshal audit entry: %w", err)
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Entry{}, fmt.Errorf("security: open audit log %s for append: %w", a.path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return Entry{}, fmt.Errorf("security: write audit log %s: %w", a.path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return Entry{}, fmt.Errorf("security: fsync audit log %s: %w", a.path, err)
	}
	if err := f.Close(); err != nil {
		return Entry{}, fmt.Errorf("security: close audit log %s: %w", a.path, err)
	}
	if err := writeHead(a.headPath, a.key, e.Seq, e.Hash); err != nil {
		return Entry{}, err
	}
	a.lastSeq = e.Seq
	a.lastHash = e.Hash
	return e, nil
}

// Verify re-reads the log file from disk, recomputes every hash and
// checks the PrevHash linkage, then checks the log against the signed
// head checkpoint: a log truncated behind the head, or an entry
// rewritten at the head's sequence, fails. It returns nil only if
// the whole chain is intact. It takes the read lock, so it never
// blocks Append; a partially written last line from a concurrent
// Append is tolerated (the log is verified up to the last complete
// entry).
func (a *AuditLog) Verify() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, err := a.readAndVerify(true)
	return err
}

// Entries returns all entries in the log in file order. The returned
// slice is a fresh copy; the caller may mutate it freely. It takes
// the read lock, so it never blocks Append; a partially written last
// line from a concurrent Append is omitted, not an error. A log
// truncated behind its signed head is an error, not a shorter list.
func (a *AuditLog) Entries() ([]Entry, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	entries, err := a.readAndVerify(true)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []Entry{}
	}
	return entries, nil
}

// Path returns the filesystem path of the log file.
func (a *AuditLog) Path() string { return a.path }
