// Per-chat session state for the Telegram command framework.
//
// Conversation memory itself is daemon-global (one memory.json), but
// titles, footer preference, usage totals and undo history are
// per-chat and live here. Sessions persist to a JSON file under the
// app's DataDir (sessions.json, 0600, atomic temp-file + rename): a
// title set with /title and a footer switched off with /footer off
// survive a daemon restart. A missing file means defaults; a corrupt
// file means defaults too (the load error is kept for the caller to
// log) — a bad sessions file must never stop the daemon starting.
// The pause flag is daemon-wide, not per chat, and persists the same
// way.
package messaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Session is one chat's command-framework state.
type Session struct {
	ChatID int64 `json:"chat_id"`
	// Title is the /title name for this session ("" = untitled).
	Title string `json:"title,omitempty"`
	// FooterEnabled controls the runtime footer on final replies.
	// Default true (see NewSessionStore's footerDefault).
	FooterEnabled bool `json:"footer_enabled"`
	// Paused is a per-chat copy of the daemon-wide pause flag at the
	// time the session was read; the authoritative flag lives in
	// SessionStore.paused. It is not persisted per session.
	Paused bool `json:"-"`
	// TotalTokens and Turns accumulate finished turns for this chat.
	TotalTokens int `json:"total_tokens"`
	Turns       int `json:"turns"`
	// LastUserInput is the last non-command message from this chat,
	// for /retry. It is kept in memory only: replaying a stale
	// input after a restart would be a surprise, and it is not
	// needed for the footer/title persistence contract.
	LastUserInput string `json:"-"`
	// UndoStack holds user texts removed by /undo, most recent last.
	// Memory only, like LastUserInput.
	UndoStack []string `json:"-"`
}

// persistedSessions is the on-disk shape for the session file.
type persistedSessions struct {
	Version  int                `json:"version"`
	Paused   bool               `json:"paused"`
	Sessions map[int64]*Session `json:"sessions"`
}

// SessionStore holds all chat sessions plus the daemon-wide pause
// flag, with JSON persistence. All methods are safe for concurrent
// use. Get returns a copy, so callers can read fields without holding
// the lock; every mutation goes through a method that persists.
type SessionStore struct {
	mu            sync.RWMutex
	path          string
	footerDefault bool
	sessions      map[int64]*Session
	paused        bool
	loadErr       error
}

// NewSessionStore opens the store rooted at path (e.g.
// <DataDir>/sessions.json). footerDefault is the FooterEnabled value
// for chats with no stored override; it comes from
// config.TelegramFooter (nil = on). A missing file starts empty. A
// corrupt file starts with defaults and records the error — read it
// with LoadError and log it; the store itself never fails to build.
func NewSessionStore(path string, footerDefault bool) *SessionStore {
	s := &SessionStore{
		path:          path,
		footerDefault: footerDefault,
		sessions:      map[int64]*Session{},
	}
	s.loadErr = s.load()
	return s
}

// LoadError returns the error from loading the session file at
// construction, or nil when the file loaded (or was absent).
func (s *SessionStore) LoadError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr
}

// load reads the session file into the store. Callers must not hold
// s.mu (it takes the write lock). Missing file = no error.
func (s *SessionStore) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("messaging: read sessions %q: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var p persistedSessions
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("messaging: corrupt sessions %q: %w", s.path, err)
	}
	if p.Version != 0 && p.Version != 1 {
		return fmt.Errorf("messaging: unsupported sessions version %d in %q", p.Version, s.path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Sessions != nil {
		s.sessions = p.Sessions
	}
	s.paused = p.Paused
	// Normalise: every stored session gets its ChatID from the key
	// and a defined footer flag.
	for id, sess := range s.sessions {
		if sess == nil {
			s.sessions[id] = &Session{ChatID: id, FooterEnabled: s.footerDefault}
			continue
		}
		sess.ChatID = id
	}
	return nil
}

// saveLocked writes the store atomically (temp file 0600 in the same
// directory + rename). Callers must hold s.mu (write).
func (s *SessionStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("messaging: create sessions dir: %w", err)
	}
	p := persistedSessions{
		Version:  1,
		Paused:   s.paused,
		Sessions: s.sessions,
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("messaging: encode sessions: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("messaging: create temp sessions file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("messaging: chmod temp sessions file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("messaging: write temp sessions file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("messaging: sync temp sessions file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("messaging: close temp sessions file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("messaging: rename temp sessions file: %w", err)
	}
	return nil
}

// Get returns a copy of the session for chatID, creating it with
// defaults (FooterEnabled = footerDefault) when new. The copy is
// safe to read without the lock; mutate through the store's methods.
func (s *SessionStore) Get(chatID int64) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[chatID]; ok && sess != nil {
		cp := *sess
		cp.Paused = s.paused
		cp.UndoStack = append([]string(nil), sess.UndoStack...)
		return &cp
	}
	sess := &Session{ChatID: chatID, FooterEnabled: s.footerDefault, Paused: s.paused}
	s.sessions[chatID] = sess
	return &Session{ChatID: chatID, FooterEnabled: s.footerDefault, Paused: s.paused}
}

// SetTitle sets the session title and persists it.
func (s *SessionStore) SetTitle(chatID int64, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessionLocked(chatID)
	sess.Title = title
	return s.saveLocked()
}

// SetFooter sets the per-chat footer preference and persists it.
func (s *SessionStore) SetFooter(chatID int64, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessionLocked(chatID)
	sess.FooterEnabled = on
	return s.saveLocked()
}

// AddUsage adds tokens to the chat's totals and counts one turn, then
// persists. It is best-effort from the caller's view: the error is
// returned so the caller can log it, but a failed usage write must
// never fail the turn that just completed.
func (s *SessionStore) AddUsage(chatID int64, tokens int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessionLocked(chatID)
	sess.TotalTokens += tokens
	sess.Turns++
	return s.saveLocked()
}

// SetLastInput records the last non-command user message (memory
// only, not persisted).
func (s *SessionStore) SetLastInput(chatID int64, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionLocked(chatID).LastUserInput = text
}

// PushUndo appends undone user texts to the session's undo stack
// (memory only, not persisted).
func (s *SessionStore) PushUndo(chatID int64, texts ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessionLocked(chatID)
	sess.UndoStack = append(sess.UndoStack, texts...)
}

// Reset clears the memory-only turn state (last input, undo stack)
// after /new. The title and footer preference are kept: starting a
// new conversation does not erase how the chat likes its replies.
func (s *SessionStore) Reset(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessionLocked(chatID)
	sess.LastUserInput = ""
	sess.UndoStack = nil
}

// SetPaused sets the daemon-wide pause flag and persists it.
func (s *SessionStore) SetPaused(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = on
	return s.saveLocked()
}

// IsPaused reports the daemon-wide pause flag.
func (s *SessionStore) IsPaused() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused
}

// Totals sums TotalTokens and Turns across all chats (daemon-wide
// usage for /usage).
func (s *SessionStore) Totals() (tokens, turns int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess == nil {
			continue
		}
		tokens += sess.TotalTokens
		turns += sess.Turns
	}
	return tokens, turns
}

// sessionLocked returns the stored session for chatID, creating it
// with defaults when new. Callers must hold s.mu (write).
func (s *SessionStore) sessionLocked(chatID int64) *Session {
	sess, ok := s.sessions[chatID]
	if !ok || sess == nil {
		sess = &Session{ChatID: chatID, FooterEnabled: s.footerDefault}
		s.sessions[chatID] = sess
	}
	return sess
}
