// Package memory — EphemeralStore implementation.
//
// EphemeralStore is a Store that keeps everything in memory and never
// touches disk. It exists for subagent child agents: a child runs its
// own agent loop with its own conversation history, and that history
// must never pollute the parent's durable memory (nor be persisted
// anywhere). Save and Close are no-ops that always succeed — there is
// nothing to persist — and Close is idempotent, like FileStore's.
//
// Semantics mirror FileStore exactly (same MaxMessages cap, same
// key-trimming and timestamp rules for facts, same RecentMessages
// contract) so a child agent behaves identically to a parent agent
// except that its memory vanishes when the task ends.
package memory

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/model"
)

// EphemeralStore implements Store entirely in memory.
type EphemeralStore struct {
	mu       sync.RWMutex
	facts    map[string]Fact
	messages []model.Message
	closed   bool
}

// Compile-time check that EphemeralStore satisfies Store.
var _ Store = (*EphemeralStore)(nil)

// NewEphemeralStore returns an empty in-memory store. Nothing it
// holds is ever written to disk.
func NewEphemeralStore() *EphemeralStore {
	return &EphemeralStore{
		facts:    make(map[string]Fact),
		messages: []model.Message{},
	}
}

// AddFact inserts a fact or updates an existing one. On update the
// original CreatedAt is preserved and UpdatedAt is refreshed. An empty
// key is an error. The key is stored trimmed, matching FileStore.
func (s *EphemeralStore) AddFact(key, value string) error {
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
	return nil
}

// GetFact returns the fact for key, and whether it was found. The
// key is trimmed before lookup, matching how AddFact stores it.
func (s *EphemeralStore) GetFact(key string) (Fact, bool, error) {
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
// how AddFact stores it.
func (s *EphemeralStore) DeleteFact(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	delete(s.facts, strings.TrimSpace(key))
	return nil
}

// SearchFacts returns facts whose key or value contains query as a
// case-insensitive substring, most recently updated first. An empty
// query matches every fact.
func (s *EphemeralStore) SearchFacts(query string) ([]Fact, error) {
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
func (s *EphemeralStore) ListFacts() ([]Fact, error) {
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
// oldest messages so history never exceeds MaxMessages.
func (s *EphemeralStore) AddMessage(msg model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	s.messages = append(s.messages, msg)
	if len(s.messages) > MaxMessages {
		// Copy on truncate: re-slicing would keep the dropped
		// messages alive via the backing array.
		trimmed := make([]model.Message, MaxMessages)
		copy(trimmed, s.messages[len(s.messages)-MaxMessages:])
		s.messages = trimmed
	}
	return nil
}

// RecentMessages returns the last n messages in chronological order.
// n <= 0 returns the whole retained history (at most MaxMessages).
func (s *EphemeralStore) RecentMessages(n int) ([]model.Message, error) {
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
// first, same as AddMessage). A nil msgs clears the history.
func (s *EphemeralStore) ReplaceMessages(msgs []model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	cp := make([]model.Message, len(msgs))
	copy(cp, msgs)
	if len(cp) > MaxMessages {
		trimmed := make([]model.Message, MaxMessages)
		copy(trimmed, cp[len(cp)-MaxMessages:])
		cp = trimmed
	}
	s.messages = cp
	return nil
}

// ClearMessages empties conversation history.
func (s *EphemeralStore) ClearMessages() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(); err != nil {
		return err
	}
	s.messages = []model.Message{}
	return nil
}

// Save is a no-op: there is nothing on disk to persist. It returns
// ErrClosed after Close, like every other operation.
func (s *EphemeralStore) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkOpenLocked()
}

// Close marks the store closed. Every subsequent operation returns
// ErrClosed. Close is idempotent. Nothing is persisted — the
// in-memory state is simply abandoned.
func (s *EphemeralStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return nil
}

// checkOpenLocked reports ErrClosed when the store is closed.
// Callers must hold s.mu (read or write).
func (s *EphemeralStore) checkOpenLocked() error {
	if s.closed {
		return ErrClosed
	}
	return nil
}
