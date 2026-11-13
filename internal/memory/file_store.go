// Package memory — FileStore implementation.
//
// FileStore is a Store backed by a single JSON file under a data
// directory. All state lives in memory under a sync.RWMutex and is
// persisted atomically (temp file in the same directory + fsync +
// rename + directory fsync) after every mutation, so a crash mid-write
// never leaves a torn memory.json behind and a completed rename is
// durable. The file is created with mode 0600 and the directory with
// mode 0700.
package memory

import (
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

// MaxMessages is the cap on retained conversation history. Older
// messages are dropped first-in-first-out when the cap is exceeded.
const MaxMessages = 200

// memoryFileName is the single persistence file inside the data dir.
const memoryFileName = "memory.json"

var (
	// ErrClosed is returned by every operation on a store after Close.
	ErrClosed = errors.New("memory: store is closed")
	// ErrEmptyKey is returned when a fact key is empty.
	ErrEmptyKey = errors.New("memory: fact key must not be empty")
)

// persisted is the on-disk JSON shape for a FileStore.
type persisted struct {
	Version  int             `json:"version"`
	Facts    []Fact          `json:"facts"`
	Messages []model.Message `json:"messages"`
}

// FileStore implements Store backed by one JSON file.
type FileStore struct {
	mu       sync.RWMutex
	dataDir  string
	filePath string
	facts    map[string]Fact
	messages []model.Message
	closed   bool
}

// Compile-time check that FileStore satisfies Store.
var _ Store = (*FileStore)(nil)

// NewFileStore opens (or creates) a FileStore rooted at dataDir. It
// creates the directory (0700) when missing and loads memory.json when
// present. A corrupt existing file is an error — it is never silently
// discarded. An empty file is treated as a fresh store.
//
// Only directories this call actually creates get their mode enforced
// (0700). A pre-existing directory is left exactly as it is: the user
// may point data_dir at $HOME or another shared directory, and
// silently chmodding that would be a permissions surprise.
func NewFileStore(dataDir string) (*FileStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("memory: data directory must not be empty")
	}
	// Record which ancestors do not exist yet, before MkdirAll
	// creates them: only those get chmodded to 0700 below. A Stat
	// failure other than "not exist" stops the walk and lets
	// MkdirAll surface the real error.
	var created []string
	for d := dataDir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			break
		}
		created = append(created, d)
		if parent := filepath.Dir(d); parent == d {
			break
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create data dir %q: %w", dataDir, err)
	}
	// MkdirAll applies the mode subject to umask, so enforce 0700
	// exactly — but only on the directories this call created.
	for _, d := range created {
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, fmt.Errorf("memory: chmod data dir %q: %w", d, err)
		}
	}

	fs := &FileStore{
		dataDir:  dataDir,
		filePath: filepath.Join(dataDir, memoryFileName),
		facts:    make(map[string]Fact),
		messages: []model.Message{},
	}

	data, err := os.ReadFile(fs.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fs, nil
		}
		return nil, fmt.Errorf("memory: read %q: %w", fs.filePath, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return fs, nil
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("memory: corrupt file %q: %w", fs.filePath, err)
	}
	// Only version 1 (and version 0, written before the field existed)
	// is understood. A newer version means the file was written by a
	// newer Orion; refuse rather than silently misreading it.
	if p.Version != 0 && p.Version != 1 {
		return nil, fmt.Errorf("memory: unsupported memory file version %d in %q (supported: 0, 1)", p.Version, fs.filePath)
	}
	for _, f := range p.Facts {
		// Store the trimmed key: a fact saved as " key " must be
		// the same fact as "key", including for files written
		// before keys were trimmed on write.
		f.Key = strings.TrimSpace(f.Key)
		if f.Key == "" {
			continue
		}
		fs.facts[f.Key] = f
	}
	if p.Messages != nil {
		fs.messages = p.Messages
		// Enforce the cap on load, keeping the most recent messages.
		// Copy on truncate: re-slicing would keep the dropped
		// messages alive via the backing array.
		if len(fs.messages) > MaxMessages {
			trimmed := make([]model.Message, MaxMessages)
			copy(trimmed, fs.messages[len(fs.messages)-MaxMessages:])
			fs.messages = trimmed
		}
	}
	return fs, nil
}

// AddFact inserts a fact or updates an existing one. On update the
// original CreatedAt is preserved and UpdatedAt is refreshed. An empty
// key is an error. The key is stored trimmed, so " key " and "key"
// are the same fact. The change is persisted immediately.
func (s *FileStore) AddFact(key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return ErrEmptyKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	now := time.Now().UTC()
	fact, exists := s.facts[key]
	if exists {
		fact.Value = value
		fact.UpdatedAt = now
	} else {
		fact = Fact{Key: key, Value: value, CreatedAt: now, UpdatedAt: now}
	}
	s.facts[key] = fact
	return s.saveLocked()
}

// GetFact returns the fact for key, and whether it was found. The
// key is trimmed before lookup, matching how AddFact stores it.
func (s *FileStore) GetFact(key string) (Fact, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkOpenLocked(); err != nil {
		return Fact{}, false, err
	}
	fact, ok := s.facts[strings.TrimSpace(key)]
	return fact, ok, nil
}

// DeleteFact removes a fact. Deleting a key that does not exist is a
// no-op and not an error. The key is trimmed before lookup, matching
// how AddFact stores it. The change is persisted immediately.
func (s *FileStore) DeleteFact(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	if _, ok := s.facts[strings.TrimSpace(key)]; !ok {
		return nil
	}
	delete(s.facts, strings.TrimSpace(key))
	return s.saveLocked()
}

// SearchFacts returns facts whose key or value contains query as a
// case-insensitive substring, most recently updated first. An empty
// query matches every fact.
func (s *FileStore) SearchFacts(query string) ([]Fact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	out := make([]Fact, 0, len(s.facts))
	for _, f := range s.facts {
		if q == "" ||
			strings.Contains(strings.ToLower(f.Key), q) ||
			strings.Contains(strings.ToLower(f.Value), q) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].Key < out[j].Key
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// ListFacts returns all facts sorted by key.
func (s *FileStore) ListFacts() ([]Fact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	out := make([]Fact, 0, len(s.facts))
	for _, f := range s.facts {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// AddMessage appends a message to conversation history, dropping the
// oldest messages so history never exceeds MaxMessages. The change
// is persisted immediately.
func (s *FileStore) AddMessage(msg model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	s.messages = append(s.messages, msg)
	if len(s.messages) > MaxMessages {
		// Copy on truncate: re-slicing would keep the dropped
		// messages (and their tool-call data) alive via the
		// backing array.
		trimmed := make([]model.Message, MaxMessages)
		copy(trimmed, s.messages[len(s.messages)-MaxMessages:])
		s.messages = trimmed
	}
	return s.saveLocked()
}

// RecentMessages returns the last n messages in chronological order.
// n <= 0 returns the whole retained history (at most MaxMessages).
func (s *FileStore) RecentMessages(n int) ([]model.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	if n <= 0 || n >= len(s.messages) {
		out := make([]model.Message, len(s.messages))
		copy(out, s.messages)
		return out, nil
	}
	out := make([]model.Message, n)
	copy(out, s.messages[len(s.messages)-n:])
	return out, nil
}

// ReplaceMessages replaces the whole retained conversation history
// with a copy of msgs, enforcing the MaxMessages cap (oldest dropped
// first, same as AddMessage). A nil msgs clears the history. The
// change is persisted immediately.
func (s *FileStore) ReplaceMessages(msgs []model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	cp := make([]model.Message, len(msgs))
	copy(cp, msgs)
	if len(cp) > MaxMessages {
		// Copy on truncate: re-slicing would keep the dropped
		// messages alive via the backing array.
		trimmed := make([]model.Message, MaxMessages)
		copy(trimmed, cp[len(cp)-MaxMessages:])
		cp = trimmed
	}
	s.messages = cp
	return s.saveLocked()
}

// ClearMessages empties conversation history. The change is persisted
// immediately.
func (s *FileStore) ClearMessages() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	s.messages = []model.Message{}
	return s.saveLocked()
}

// Save persists the current in-memory state to disk atomically.
func (s *FileStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	return s.saveLocked()
}

// Close persists the current state and marks the store closed. Every
// subsequent operation returns ErrClosed. Close is idempotent.
//
// If the final save fails, Close returns the error and the store stays
// open, so the save can be retried (call Save or Close again) instead
// of silently losing the in-memory state. One consequence callers must
// know about: a caller that calls Save and then Close (as
// Agent.Shutdown does) sees the same save failure reported twice —
// once from each call — because each call genuinely re-attempts the
// save. The store cannot dedupe that: it has no way to know the error
// was already reported to the caller, and swallowing the error in
// Close would hide a failed persist from callers that only call
// Close. Deduping the double report belongs to the caller.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// checkOpenLocked reports ErrClosed when the store is closed.
// Callers must hold s.mu (read or write).
func (s *FileStore) checkOpenLocked() error {
	if s.closed {
		return ErrClosed
	}
	return nil
}

// saveLocked writes state to disk atomically: encode to a temp file in
// the same directory (0600), fsync it, then rename over memory.json.
// Callers must hold s.mu (write).
func (s *FileStore) saveLocked() error {
	facts := make([]Fact, 0, len(s.facts))
	for _, f := range s.facts {
		facts = append(facts, f)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Key < facts[j].Key })
	msgs := make([]model.Message, len(s.messages))
	copy(msgs, s.messages)
	if msgs == nil {
		msgs = []model.Message{}
	}
	p := persisted{Version: 1, Facts: facts, Messages: msgs}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("memory: encode state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dataDir, ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any failure path; after a successful
	// rename the temp path no longer exists and Remove is a no-op.
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.filePath); err != nil {
		return fmt.Errorf("memory: rename temp file to %q: %w", s.filePath, err)
	}
	// Fsync the directory so the rename itself is durable: without
	// this a crash (or kill -9 followed by a power loss) could roll
	// memory.json back to its previous contents even though the
	// rename returned success.
	if err := syncDir(s.dataDir); err != nil {
		return fmt.Errorf("memory: sync data dir %q: %w", s.dataDir, err)
	}
	return nil
}

// syncDir fsyncs a directory so renames into it survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
