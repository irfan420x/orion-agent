// Package session provides Orion's per-session transcript: one
// append-only JSONL file per session.
//
// A session is one continuous conversation. Its transcript is the
// source of truth for turns: every message the agent loop stores is
// also appended here, with a per-message timestamp, and nothing is
// ever rewritten or deleted from it — compaction only ever rewrites
// the working history in memory.json, never the transcript. That is
// what makes the transcript the backstop for the old silent-loss
// path (the 200-message FIFO cap used to drop turns with no summary
// and no trace): even if auto-compaction trips its circuit breaker
// and the cap drops a turn from memory.json, the turn is still in
// the transcript and can be resumed or read back.
//
// Layout (one directory per owner — a chat's directory under
// <DataDir>/chats/<chatID>, or <DataDir> itself for the CLI):
//
//	sessions/<session-id>.jsonl   one file per session
//	sessions/current.json         {"session_id": "<id>"} — the live session
//
// A session id is "<UTC timestamp>-<8 hex>"; ids sort
// chronologically and are safe as file names. The transcript file
// is 0600, the directory 0700. Appends are O_APPEND + fsync, so a
// crash mid-append cannot tear an earlier record. A corrupt line
// (torn write, hand edit) is skipped and counted, never fatal: one
// bad line must not lose the whole session.
//
// Migration: a transcript that is opened empty over a store that
// already holds turns imports those turns once (MigrateFrom),
// stamped with the source file's modification time and marked
// Migrated, so pre-transcript history is never silently discarded
// and never imported twice. Typed memories and facts are untouched
// by this package — they live in internal/memory and stay shared.
package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
)

// RecordKind distinguishes transcript records.
type RecordKind string

const (
	// KindMessage is one conversation message (user, assistant,
	// tool) exactly as stored in memory.
	KindMessage RecordKind = "message"
	// KindSummary is an auto- or manual-compaction summary: the
	// text that was saved before older turns were dropped from
	// the working history.
	KindSummary RecordKind = "summary"
	// KindDropped is a turn the store's FIFO cap dropped without
	// a summary (auto-compaction unavailable): the turn's role
	// and truncated content, so the loss is explicit in the
	// transcript instead of silent (Audit 5 F-A5-1).
	KindDropped RecordKind = "dropped"
)

// Record is one JSONL line in a session transcript.
type Record struct {
	SessionID string         `json:"session_id"`
	Seq       int            `json:"seq"`
	Timestamp time.Time      `json:"timestamp"`
	Kind      RecordKind     `json:"kind"`
	Message   *model.Message `json:"message,omitempty"`
	Text      string         `json:"text,omitempty"`
	Migrated  bool           `json:"migrated,omitempty"`
}

// Info describes one session on disk, for listing.
type Info struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  int
	Summaries int
	Dropped   int // turns dropped without a summary (KindDropped)
	Bytes     int64
	Title     string // first user message, truncated
}

// currentFileName is the live-session pointer inside a sessions dir.
const currentFileName = "current.json"

// currentPointer is the on-disk shape of current.json.
type currentPointer struct {
	SessionID string `json:"session_id"`
}

// NewID returns a fresh session id: UTC timestamp plus 8 random hex
// digits. The timestamp makes ids sort chronologically; the random
// suffix makes two sessions started in the same second distinct.
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is vanishingly unlikely; fall back
		// to a nanosecond suffix rather than failing session
		// creation outright.
		return time.Now().UTC().Format("20060102-150405") + fmt.Sprintf("-%08x", time.Now().UnixNano()&0xffffffff)
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// ValidateID reports whether id is safe to use as a file name: the
// shape NewID produces, nothing else. Resume-by-prefix resolves
// against listed ids first and never touches the filesystem with
// raw user input, but any direct caller still gets this check.
func ValidateID(id string) bool {
	if len(id) < len("20060102-150405-") {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || r == '-' {
			continue
		}
		return false
	}
	return true
}

// SessionsDir returns the sessions directory under root (a chat
// directory, or the data dir for the CLI).
func SessionsDir(root string) string { return filepath.Join(root, "sessions") }

// ReadCurrent returns the live session id recorded in root's
// sessions/current.json, or "" when none is recorded yet.
func ReadCurrent(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(SessionsDir(root), currentFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("session: read current pointer: %w", err)
	}
	var p currentPointer
	if err := json.Unmarshal(data, &p); err != nil {
		return "", fmt.Errorf("session: corrupt current pointer: %w", err)
	}
	return strings.TrimSpace(p.SessionID), nil
}

// WriteCurrent records id as root's live session, atomically
// (temp file 0600 + rename + dir fsync).
func WriteCurrent(root, id string) error {
	if !ValidateID(id) {
		return fmt.Errorf("session: invalid session id %q", id)
	}
	dir := SessionsDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("session: create sessions dir %q: %w", dir, err)
	}
	data, err := json.Marshal(currentPointer{SessionID: id})
	if err != nil {
		return fmt.Errorf("session: encode current pointer: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".current-*.tmp")
	if err != nil {
		return fmt.Errorf("session: create temp current pointer: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("session: chmod temp current pointer: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("session: write temp current pointer: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("session: sync temp current pointer: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close temp current pointer: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, currentFileName)); err != nil {
		return fmt.Errorf("session: rename current pointer: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// maxTranscriptLineBytes caps one JSONL line: a longer line is
// treated as one corrupt line and skipped, never fatal — a single
// overlong line must not make the whole session unloadable
// (Audit 5 F-A5-P2-3). 16 MiB is far above any legitimate record
// (tool results are capped at 256 KiB) and far below an OOM.
const maxTranscriptLineBytes = 16 * 1024 * 1024

// openTranscripts tracks transcripts open in this process, keyed
// by absolute path. Open returns the existing handle for a path
// that is already open here — one file, one sequence counter — so
// two handles can never assign the same seq (Audit 5 F-A5-P2-6).
// Across processes the flock on the held fd enforces the same
// single-owner discipline: a second process's Open fails instead
// of interleaving records with duplicate seqs.
var openTranscriptsMu sync.Mutex
var openTranscripts = map[string]*Transcript{}

// Transcript is one session's append-only JSONL file. It is safe
// for concurrent use.
//
// The file is held open for the handle's life (O_APPEND) under an
// exclusive flock, taken on Open and released on Close: the lock
// serialises writers across processes, and the held fd means the
// in-memory seq/count can never drift from the file. Close when a
// handle is no longer needed (session rotation/resume); a handle
// that is never closed keeps its lock until the process exits.
// Open returns the already-open in-process handle for the same
// path (reference-counted), so reopening a live session in this
// process — a daemon restart in-process, a CLI sharing the App —
// never conflicts with itself.
type Transcript struct {
	mu      sync.Mutex
	dir     string
	id      string
	path    string
	f       *os.File // held O_APPEND fd, flocked LOCK_EX
	refs    int      // in-process Open references; 0 releases lock + fd
	closed  bool
	seq     int
	count   int
	corrupt int
}

// Open opens (or creates) the transcript for id under root's
// sessions directory. The directory is created 0700 when missing;
// the file is created 0600 on first open, so a fresh session is
// listable (and its id resolvable) before its first turn is
// recorded — /new rotates to a session that /sessions must show.
// Existing records are counted and the sequence resumes after the
// highest seen seq, so reopening after a restart never reuses a
// sequence number. A corrupt line is skipped and counted (see
// CorruptLines), never fatal. Opening a transcript another process
// already holds is an error: one owner per file.
//
// An already-open path in this process returns the existing
// handle (with its reference count raised); call Close once per
// Open when the handle is no longer needed.
func Open(root, id string) (*Transcript, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("session: root directory must not be empty")
	}
	if !ValidateID(id) {
		return nil, fmt.Errorf("session: invalid session id %q", id)
	}
	dir := SessionsDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: create sessions dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, id+".jsonl")
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	openTranscriptsMu.Lock()
	defer openTranscriptsMu.Unlock()
	if tr, ok := openTranscripts[abs]; ok {
		tr.mu.Lock()
		tr.refs++
		tr.mu.Unlock()
		return tr, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session: open transcript %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("session: transcript %q is already open in another process: %w", path, err)
	}
	t := &Transcript{dir: dir, id: id, path: path, f: f, refs: 1}
	if err := t.scanLocked(); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	openTranscripts[abs] = t
	return t, nil
}

// Close releases one Open reference to the transcript. When the
// last reference closes, the flock is released and the held fd
// closed; the file itself is untouched. Appends after the last
// close fail; reads still work (they re-read the file). Close is
// idempotent and safe to call on a nil transcript.
func (t *Transcript) Close() error {
	if t == nil {
		return nil
	}
	abs, err := filepath.Abs(t.path)
	if err != nil {
		abs = t.path
	}
	openTranscriptsMu.Lock()
	defer openTranscriptsMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.refs--
	if t.refs > 0 {
		return nil
	}
	t.closed = true
	delete(openTranscripts, abs)
	if t.f != nil {
		syscall.Flock(int(t.f.Fd()), syscall.LOCK_UN)
		err := t.f.Close()
		t.f = nil
		return err
	}
	return nil
}

// ID returns the session id.
func (t *Transcript) ID() string { return t.id }

// Path returns the transcript file path.
func (t *Transcript) Path() string { return t.path }

// Count returns how many records the transcript holds (messages +
// summaries).
func (t *Transcript) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count
}

// CorruptLines returns how many lines in the file could not be
// parsed when it was last scanned (Open, or the last read).
func (t *Transcript) CorruptLines() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.corrupt
}

// scanLines reads f line by line, calling fn for each line
// (without its trailing newline). A line longer than
// maxTranscriptLineBytes is reported once with overlong=true and
// empty bytes — the rest of the line is consumed so scanning
// resumes at the next line, and the line is one corrupt line, not
// a fatal error. The final line need not end with a newline. The
// read is bounded: an overlong line never accumulates in memory.
//
// Callers MUST check overlong before any emptiness test: an
// overlong line arrives with nil bytes, so a TrimSpace-then-empty
// check would silently swallow it without counting it (Audit 6
// F-A6-P2-4).
func scanLines(f *os.File, fn func(line []byte, overlong bool) error) error {
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, overlong, done, err := readLine(r)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if err := fn(line, overlong); err != nil {
			return err
		}
	}
}

// readLine reads one line from r. done is true when the file is
// exhausted with no line left. Overlong lines are consumed in
// fragments and reported with overlong=true and no bytes.
func readLine(r *bufio.Reader) (line []byte, overlong bool, done bool, err error) {
	for {
		frag, rerr := r.ReadSlice('\n')
		if len(frag) > 0 && !overlong {
			if len(line)+len(frag) > maxTranscriptLineBytes {
				overlong = true
				line = nil
			} else {
				line = append(line, frag...)
			}
		}
		switch rerr {
		case nil:
			// Complete line: strip the trailing newline.
			if n := len(line); n > 0 && line[n-1] == '\n' {
				line = line[:n-1]
			}
			return line, overlong, false, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 && !overlong {
				return nil, false, true, nil
			}
			return line, overlong, false, nil
		default:
			return nil, false, false, rerr
		}
	}
}

// scanLocked recounts the file: highest seq, record count, corrupt
// lines. Callers must hold t.mu.
func (t *Transcript) scanLocked() error {
	t.seq, t.count, t.corrupt = 0, 0, 0
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("session: open transcript %q: %w", t.path, err)
	}
	defer f.Close()
	err = scanLines(f, func(line []byte, overlong bool) error {
		// Overlong first: the line arrives with nil bytes, so
		// the emptiness check below would swallow it uncounted
		// (Audit 6 F-A6-P2-4).
		if overlong {
			t.corrupt++
			return nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return nil
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			t.corrupt++
			return nil
		}
		t.count++
		if r.Seq > t.seq {
			t.seq = r.Seq
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("session: scan transcript %q: %w", t.path, err)
	}
	return nil
}

// rescanLocked re-derives seq/count/corrupt from the file after a
// failed append: the bytes may have landed despite the error, so
// the in-memory counters must reflect the file, never a guess.
// A scan failure leaves the counters as they were (a gap in seq is
// safe; a reuse is not).
func (t *Transcript) rescanLocked() {
	_ = t.scanLocked()
}

// recordsLocked reads every parseable record in order. Corrupt
// lines are skipped and counted; only a read error fails the call.
// Callers must hold t.mu.
func (t *Transcript) recordsLocked() ([]Record, int, error) {
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("session: open transcript %q: %w", t.path, err)
	}
	defer f.Close()
	var out []Record
	corrupt := 0
	err = scanLines(f, func(line []byte, overlong bool) error {
		// Overlong first: the line arrives with nil bytes, so
		// the emptiness check below would swallow it uncounted
		// (Audit 6 F-A6-P2-4).
		if overlong {
			corrupt++
			return nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return nil
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			corrupt++
			return nil
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("session: scan transcript %q: %w", t.path, err)
	}
	return out, corrupt, nil
}

// scrubRecord redacts secrets from a record before it is
// persisted (Audit 5 F-A5-P2-8): the transcript is append-only and
// keeps every turn forever, so a pasted key or token must never
// land in it verbatim. Message content and tool-call arguments are
// scrubbed; roles, tool names and ids are not secret-bearing.
//
// The ToolCalls slice is COPIED before scrubbing (rc13): the
// caller's Message shares its backing array with the agent history
// and the memory store, and scrubbing the arguments in place
// mutated the caller's data — racing with a concurrent FileStore
// Save marshal of that same array (data race found by -race in
// TestApprovalCallbackWhileTurnSlotsFull: Shutdown saves while a
// turn is still appending; the race is pre-existing, it reproduces
// on rc12 at 3/10 runs). Content is assigned on the caller's local
// struct copy, so it needs no copy; the slice elements do.
func scrubRecord(r *Record) {
	switch r.Kind {
	case KindMessage:
		if r.Message != nil {
			r.Message.Content = security.RedactSecrets(r.Message.Content)
			if len(r.Message.ToolCalls) > 0 {
				tc := make([]model.ToolCall, len(r.Message.ToolCalls))
				copy(tc, r.Message.ToolCalls)
				r.Message.ToolCalls = tc
				for i := range tc {
					tc[i].Arguments = security.RedactSecrets(tc[i].Arguments)
				}
			}
		}
	case KindSummary, KindDropped:
		r.Text = security.RedactSecrets(r.Text)
	}
}

// appendLocked writes one record (seq assigned here, timestamp
// already set by the caller) as a single JSON line, fsynced, on
// the held fd. Callers must hold t.mu.
//
// A write/sync failure never decrements seq: the bytes may already
// be on disk, and reusing the seq would duplicate it. Instead the
// file is re-scanned so seq/count reflect what is actually stored.
func (t *Transcript) appendLocked(r Record) error {
	if t.closed || t.f == nil {
		return errors.New("session: transcript is closed")
	}
	t.seq++
	r.Seq = t.seq
	r.SessionID = t.id
	scrubRecord(&r)
	data, err := json.Marshal(r)
	if err != nil {
		t.seq--
		return fmt.Errorf("session: encode transcript record: %w", err)
	}
	data = append(data, '\n')
	if _, err := t.f.Write(data); err != nil {
		t.rescanLocked()
		return fmt.Errorf("session: append to transcript %q: %w", t.path, err)
	}
	if err := t.f.Sync(); err != nil {
		t.rescanLocked()
		return fmt.Errorf("session: sync transcript %q: %w", t.path, err)
	}
	t.count++
	return nil
}

// AppendMessage appends one conversation message with a
// per-message UTC timestamp. It never rewrites history.
func (t *Transcript) AppendMessage(msg model.Message) error {
	if t == nil {
		return errors.New("session: transcript is nil")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := msg
	return t.appendLocked(Record{
		Timestamp: time.Now().UTC(),
		Kind:      KindMessage,
		Message:   &m,
	})
}

// AppendSummary appends a compaction summary record: the text
// saved before older turns were dropped from the working history.
// An empty summary is an error — a summary record must say
// something, or there is nothing to preserve.
func (t *Transcript) AppendSummary(text string) error {
	if t == nil {
		return errors.New("session: transcript is nil")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("session: summary must not be empty")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.appendLocked(Record{
		Timestamp: time.Now().UTC(),
		Kind:      KindSummary,
		Text:      text,
	})
}

// Records returns every parseable record in order. Corrupt lines
// are skipped and counted (see CorruptLines); only a read error
// fails the call.
func (t *Transcript) Records() ([]Record, error) {
	if t == nil {
		return nil, errors.New("session: transcript is nil")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	recs, corrupt, err := t.recordsLocked()
	if err != nil {
		return nil, err
	}
	t.corrupt = corrupt
	return recs, nil
}

// AppendDropped appends a record for a turn the store's FIFO cap
// dropped without a summary (Audit 5 F-A5-1): the text names the
// dropped turn, so the loss is explicit in the transcript instead
// of silent. An empty text is an error — a dropped record must say
// what was lost.
func (t *Transcript) AppendDropped(text string) error {
	if t == nil {
		return errors.New("session: transcript is nil")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("session: dropped record must not be empty")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.appendLocked(Record{
		Timestamp: time.Now().UTC(),
		Kind:      KindDropped,
		Text:      text,
	})
}

// Messages returns the transcript's conversation messages in
// order (summary records excluded — summaries live in the
// conversation-summary fact and in the transcript's summary
// records, not in the working history).
func (t *Transcript) Messages() ([]model.Message, error) {
	recs, err := t.Records()
	if err != nil {
		return nil, err
	}
	out := make([]model.Message, 0, len(recs))
	for _, r := range recs {
		if r.Kind == KindMessage && r.Message != nil {
			out = append(out, *r.Message)
		}
	}
	return out, nil
}

// Summaries returns the transcript's compaction summaries in order.
func (t *Transcript) Summaries() ([]string, error) {
	recs, err := t.Records()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range recs {
		if r.Kind == KindSummary && strings.TrimSpace(r.Text) != "" {
			out = append(out, r.Text)
		}
	}
	return out, nil
}

// MigrateFrom imports msgs into the transcript, once — with
// resume: when the transcript already holds records it imports
// only the remainder. Imported records are stamped with
// importedAt (the source file's modification time, so the
// timestamps say "when this history was written", not "when it
// was imported") and marked Migrated. It never deletes or modifies
// the source history; the caller keeps memory.json as-is.
//
// Resumability (Audit 5 F-A5-P2-4): a migration that failed
// mid-way used to be unrecoverable — the already-appended messages
// made the "import once" guard block every retry. Now a transcript
// holding only Migrated message records imports the store turns
// not already present (matched as a multiset on role+content, in
// order — a partial import is always a prefix of the store's
// sequence, so the remainder is exactly the turns after it).
// Compaction summaries and dropped-turn spill records are
// boundaries, not blockers (Audit 6 F-A6-P2-5): a session that
// compacted after a partial migration still imports its remainder
// — only a live (non-migrated) message blocks migration, because
// the session has then run turns of its own and importing older
// turns after them would scramble the order. On a mid-way error
// it returns the count appended so far alongside the error, and
// the next open resumes the remainder.
func (t *Transcript) MigrateFrom(msgs []model.Message, importedAt time.Time) (int, error) {
	if t == nil {
		return 0, errors.New("session: transcript is nil")
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	if importedAt.IsZero() {
		importedAt = time.Now().UTC()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	recs, _, err := t.recordsLocked()
	if err != nil {
		return 0, err
	}
	// Only a live (non-migrated) MESSAGE blocks migration. A
	// compaction summary or a dropped-turn spill is a boundary
	// record, not a live turn: it says what happened to older
	// history, and the unmigrated remainder still belongs after
	// it (Audit 6 F-A6-P2-5). The old guard refused the whole
	// remainder on ANY non-migrated record, so one summary
	// permanently blocked the rest of the legacy history.
	for _, r := range recs {
		if r.Kind == KindMessage && !r.Migrated {
			return 0, nil
		}
	}
	// Turns already imported, as a multiset: a turn already in the
	// transcript is skipped, so a retry imports the remainder.
	have := make(map[string]int, len(recs))
	for _, r := range recs {
		if r.Message != nil {
			have[migrateKey(*r.Message)]++
		}
	}
	appended := 0
	for _, m := range msgs {
		if k := migrateKey(m); have[k] > 0 {
			have[k]--
			continue
		}
		mm := m
		if err := t.appendLocked(Record{
			Timestamp: importedAt,
			Kind:      KindMessage,
			Message:   &mm,
			Migrated:  true,
		}); err != nil {
			return appended, err
		}
		appended++
	}
	return appended, nil
}

// migrateKey identifies a turn for the import-once multiset: role
// plus content. Tool calls are part of the content for these
// purposes — two turns with the same text are the same turn.
func migrateKey(m model.Message) string {
	return m.Role + "\x00" + m.Content
}

// List returns every session under root, newest first, with
// per-session metadata read from its transcript (first/last
// timestamps, message and summary counts, title from the first
// user message). A session whose transcript cannot be read at all
// (permissions) is skipped, not fatal; a corrupt line inside a
// readable transcript only costs that line.
func List(root string) ([]Info, error) {
	dir := SessionsDir(root)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: list sessions in %q: %w", dir, err)
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if !ValidateID(id) {
			continue
		}
		info, err := statTranscript(filepath.Join(dir, e.Name()), id)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// statTranscript reads one transcript file's metadata.
func statTranscript(path, id string) (Info, error) {
	info := Info{ID: id}
	st, err := os.Stat(path)
	if err != nil {
		return info, err
	}
	info.Bytes = st.Size()
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	err = scanLines(f, func(line []byte, overlong bool) error {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return nil
		}
		if overlong {
			return nil
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil
		}
		if info.CreatedAt.IsZero() {
			info.CreatedAt = r.Timestamp
		}
		info.UpdatedAt = r.Timestamp
		switch r.Kind {
		case KindMessage:
			info.Messages++
			if info.Title == "" && r.Message != nil && r.Message.Role == "user" {
				info.Title = truncateRunes(strings.TrimSpace(r.Message.Content), 60)
			}
		case KindSummary:
			info.Summaries++
		case KindDropped:
			info.Dropped++
		}
		return nil
	})
	if err != nil {
		return info, err
	}
	if info.CreatedAt.IsZero() {
		info.CreatedAt = st.ModTime().UTC()
		info.UpdatedAt = st.ModTime().UTC()
	}
	return info, nil
}

// ResolvePrefix resolves idOrPrefix (a full id or a unique leading
// prefix) against root's sessions. It returns the full id, or an
// error naming the problem: no such session, or an ambiguous prefix
// with the matches listed. An empty argument is an error.
func ResolvePrefix(root, idOrPrefix string) (string, error) {
	idOrPrefix = strings.TrimSpace(idOrPrefix)
	if idOrPrefix == "" {
		return "", errors.New("session: provide a session id (or a leading prefix of one)")
	}
	infos, err := List(root)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", errors.New("session: no sessions yet")
	}
	var matches []string
	for _, info := range infos {
		if info.ID == idOrPrefix {
			return info.ID, nil
		}
		if strings.HasPrefix(info.ID, idOrPrefix) {
			matches = append(matches, info.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("session: no session matches %q", idOrPrefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("session: prefix %q is ambiguous (%s)", idOrPrefix, strings.Join(matches, ", "))
	}
}

// truncateRunes shortens s to at most n runes, appending "..." when
// it truncates. Truncating by runes keeps the result valid UTF-8.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
