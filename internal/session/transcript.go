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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/model"
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

// Transcript is one session's append-only JSONL file. It is safe
// for concurrent use.
type Transcript struct {
	mu      sync.Mutex
	dir     string
	id      string
	path    string
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
// CorruptLines), never fatal.
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
	t := &Transcript{dir: dir, id: id, path: filepath.Join(dir, id+".jsonl")}
	if f, err := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
		return nil, fmt.Errorf("session: create transcript %q: %w", t.path, err)
	} else if err := f.Close(); err != nil {
		return nil, fmt.Errorf("session: create transcript %q: %w", t.path, err)
	}
	if err := t.scanLocked(); err != nil {
		return nil, err
	}
	return t, nil
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.corrupt++
			continue
		}
		t.count++
		if r.Seq > t.seq {
			t.seq = r.Seq
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("session: scan transcript %q: %w", t.path, err)
	}
	return nil
}

// appendLocked writes one record (seq assigned here, timestamp
// already set by the caller) as a single JSON line, fsynced.
// Callers must hold t.mu.
func (t *Transcript) appendLocked(r Record) error {
	t.seq++
	r.Seq = t.seq
	r.SessionID = t.id
	data, err := json.Marshal(r)
	if err != nil {
		t.seq--
		return fmt.Errorf("session: encode transcript record: %w", err)
	}
	data = append(data, '\n')
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.seq--
		return fmt.Errorf("session: open transcript %q: %w", t.path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		t.seq--
		return fmt.Errorf("session: append to transcript %q: %w", t.path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		t.seq--
		return fmt.Errorf("session: sync transcript %q: %w", t.path, err)
	}
	if err := f.Close(); err != nil {
		t.seq--
		return fmt.Errorf("session: close transcript %q: %w", t.path, err)
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
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: open transcript %q: %w", t.path, err)
	}
	defer f.Close()
	var out []Record
	corrupt := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			corrupt++
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session: scan transcript %q: %w", t.path, err)
	}
	t.corrupt = corrupt
	return out, nil
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

// MigrateFrom imports msgs into an empty transcript, once: when
// the transcript already holds records it returns 0, nil —
// re-importing would duplicate every turn. Imported records are
// stamped with importedAt (the source file's modification time, so
// the timestamps say "when this history was written", not "when it
// was imported") and marked Migrated. It never deletes or modifies
// the source history; the caller keeps memory.json as-is.
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
	if t.count > 0 {
		return 0, nil
	}
	for _, m := range msgs {
		mm := m
		if err := t.appendLocked(Record{
			Timestamp: importedAt,
			Kind:      KindMessage,
			Message:   &mm,
			Migrated:  true,
		}); err != nil {
			return 0, err
		}
	}
	return len(msgs), nil
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
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
		}
	}
	if err := sc.Err(); err != nil {
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
