package permissions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RuleStore persists per-tool allow/deny rules as JSON under the
// app's DataDir (permissions.json, 0600, atomic temp-file + rename +
// dir fsync — the same pattern the memory and session stores use, so
// a crash mid-write never leaves a torn rules file). A missing file
// means no rules; a corrupt file means no rules plus a load error
// the caller logs — a bad rules file must never stop the daemon
// starting, and must never silently grant anything (fail closed: no
// rules, so every gated call prompts).
type RuleStore struct {
	mu      sync.RWMutex
	path    string
	rules   map[string]Action
	loadErr error
}

// NewRuleStore opens the store rooted at path (e.g.
// <DataDir>/permissions.json). An empty path builds an in-memory-only
// store (used by tests and by hand-built apps): Set persists nowhere
// and Lookup still works.
func NewRuleStore(path string) *RuleStore {
	s := &RuleStore{path: path, rules: map[string]Action{}}
	s.loadErr = s.load()
	return s
}

// LoadError returns the error from loading the rules file at
// construction, or nil when the file loaded (or was absent).
func (s *RuleStore) LoadError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr
}

// persistedRules is the on-disk shape for the rules file.
type persistedRules struct {
	Version int               `json:"version"`
	Rules   map[string]Action `json:"rules"`
}

func (s *RuleStore) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("permissions: read rules %q: %w", s.path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var p persistedRules
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("permissions: corrupt rules %q: %w", s.path, err)
	}
	if p.Version != 0 && p.Version != 1 {
		return fmt.Errorf("permissions: unsupported rules version %d in %q", p.Version, s.path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for tool, action := range p.Rules {
		if action.Valid() && strings.TrimSpace(tool) != "" {
			s.rules[tool] = action
		}
	}
	return nil
}

// Lookup returns the standing rule for tool, if one exists.
func (s *RuleStore) Lookup(tool string) (Action, bool) {
	if s == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.rules[strings.TrimSpace(tool)]
	return a, ok
}

// Set stores a rule and persists it. An invalid action or an empty
// tool name is an error and stores nothing. The denylist stays
// absolute at this layer too: a rule cannot be the way a denylisted
// command gets approved, and Set refuses an allow rule for a tool
// whose calls the denylist can block — in practice that is
// shell_exec, whose individual commands are checked per call in
// Gate.Check. (An allow rule for shell_exec would only ever approve
// non-denylisted commands; refusing it outright would make the
// "always allow" button dead for the most common gated tool, so Set
// accepts it and the per-call denylist check in Check remains the
// absolute guard.)
func (s *RuleStore) Set(tool string, action Action) error {
	if s == nil {
		return fmt.Errorf("permissions: rule store is nil")
	}
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return fmt.Errorf("permissions: rule tool must not be empty")
	}
	if !action.Valid() {
		return fmt.Errorf("permissions: invalid rule action %q (want %q or %q)", action, ActionAllow, ActionDeny)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[tool] = action
	return s.saveLocked()
}

// Delete removes the rule for tool (a no-op when none exists) and
// persists.
func (s *RuleStore) Delete(tool string) error {
	if s == nil {
		return fmt.Errorf("permissions: rule store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rules, strings.TrimSpace(tool))
	return s.saveLocked()
}

// List returns all rules sorted by tool name.
func (s *RuleStore) List() []Rule {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rule, 0, len(s.rules))
	for tool, action := range s.rules {
		out = append(out, Rule{Tool: tool, Action: action})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}

// saveLocked writes the store atomically (temp file 0600 in the same
// directory + rename + dir fsync). Callers must hold s.mu (write).
// An in-memory-only store (empty path) is a no-op success.
func (s *RuleStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("permissions: create rules dir: %w", err)
	}
	data, err := json.MarshalIndent(persistedRules{Version: 1, Rules: s.rules}, "", "  ")
	if err != nil {
		return fmt.Errorf("permissions: encode rules: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".permissions-*.tmp")
	if err != nil {
		return fmt.Errorf("permissions: create temp rules file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("permissions: chmod temp rules file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("permissions: write temp rules file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("permissions: sync temp rules file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("permissions: close temp rules file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("permissions: rename temp rules file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// newRequestID returns 16 hex chars of cryptographic randomness for
// a pending approval request. Callback data carries it, so it must
// not be guessable or sequential: an attacker who could guess the
// next ID could approve their own tool call.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic and unrecoverable
		// here; fall back to a timestamp-derived value rather than
		// returning a duplicate ID. (In practice this never
		// happens on a working system.)
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
