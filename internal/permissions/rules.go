package permissions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
//
// Rules are per-chat (F-A16-3): a rule is keyed by (chatID, tool),
// so "Always allow shell_exec" tapped in one chat never approves
// the tool in another chat — the permission layer keeps the same
// per-chat isolation the history/model/budget layers already have.
// chatID 0 is the legacy global scope: rules written by rc21 and
// earlier (version 1 files, keyed by tool name only) carry no chat,
// so they load as global rules and keep applying everywhere rather
// than being silently dropped. Every rule written by rc22 or later
// is per-chat; the global scope exists only for those legacy
// entries (and for the pre-existing Lookup/Set API, which other
// callers use for chat-neutral rules).
type RuleStore struct {
	mu      sync.RWMutex
	path    string
	rules   map[ruleKey]Action
	loadErr error
}

// ruleKey is one rule's identity: the chat it applies to (0 =
// legacy global) plus the tool name.
type ruleKey struct {
	chatID int64
	tool   string
}

// encodeRuleKey renders a key for the on-disk map: a bare tool name
// for a global rule, "<chatID>:<tool>" for a per-chat rule. The
// version 1 file shape is therefore unchanged for global rules — a
// v1 file loads verbatim — and per-chat rules are just new keys in
// the same map.
func encodeRuleKey(k ruleKey) string {
	if k.chatID == 0 {
		return k.tool
	}
	return strconv.FormatInt(k.chatID, 10) + ":" + k.tool
}

// decodeRuleKey parses an on-disk map key. A "<digits>:<tool>" key
// is a per-chat rule; anything else is a global rule for the whole
// key as the tool name. Tool names never contain ":", so a key with
// a non-numeric prefix before the first ":" cannot be a per-chat
// key and is read as a (global) tool name instead of being dropped.
func decodeRuleKey(key string) ruleKey {
	if i := strings.Index(key, ":"); i > 0 {
		if id, err := strconv.ParseInt(key[:i], 10, 64); err == nil {
			if tool := strings.TrimSpace(key[i+1:]); tool != "" {
				return ruleKey{chatID: id, tool: tool}
			}
		}
	}
	return ruleKey{chatID: 0, tool: key}
}

// NewRuleStore opens the store rooted at path (e.g.
// <DataDir>/permissions.json). An empty path builds an in-memory-only
// store (used by tests and by hand-built apps): Set persists nowhere
// and Lookup still works.
func NewRuleStore(path string) *RuleStore {
	s := &RuleStore{path: path, rules: map[ruleKey]Action{}}
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

// persistedRules is the on-disk shape for the rules file. The
// "rules" map is keyed by encodeRuleKey: a bare tool name is a
// legacy global rule (version 1), "<chatID>:<tool>" is a per-chat
// rule (version 2). Version 1 files therefore load without any
// migration step and without losing a rule; saving writes version
// 2, which older readers (version 1) still parse — they see the
// per-chat keys as unknown tool names and ignore them, they never
// crash on the file.
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
	if p.Version != 0 && p.Version != 1 && p.Version != 2 {
		return fmt.Errorf("permissions: unsupported rules version %d in %q", p.Version, s.path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, action := range p.Rules {
		if !action.Valid() || strings.TrimSpace(key) == "" {
			continue
		}
		k := decodeRuleKey(key)
		if strings.TrimSpace(k.tool) == "" {
			continue
		}
		s.rules[k] = action
	}
	return nil
}

// Lookup returns the standing GLOBAL rule for tool, if one exists.
// It is the pre-rc22 API: global rules are the legacy scope (a
// version 1 file, or a rule set through Set). Per-chat checks must
// use LookupForChat, which consults the chat's own rule first and
// falls back to this global rule.
func (s *RuleStore) Lookup(tool string) (Action, bool) {
	return s.LookupForChat(0, tool)
}

// LookupForChat returns the standing rule for tool in chatID: the
// chat's own rule when one exists, otherwise the legacy global
// rule for the tool (a rule written before rules were per-chat
// carries no chat, so it still applies — dropping it would silently
// revoke a decision the user already made). A chat with neither
// reports no rule, so the gate prompts as before.
func (s *RuleStore) LookupForChat(chatID int64, tool string) (Action, bool) {
	if s == nil {
		return "", false
	}
	tool = strings.TrimSpace(tool)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.rules[ruleKey{chatID: chatID, tool: tool}]; ok {
		return a, true
	}
	if chatID != 0 {
		if a, ok := s.rules[ruleKey{chatID: 0, tool: tool}]; ok {
			return a, true
		}
	}
	return "", false
}

// Set stores a GLOBAL rule and persists it. An invalid action or an empty
// tool name is an error and stores nothing. This is the pre-rc22
// API, kept for chat-neutral callers (tests, the core agent loop's
// fixtures): the gate itself persists per-chat rules through
// SetForChat. The denylist stays absolute at this layer too: a rule cannot be the way a denylisted
// command gets approved, and Set refuses an allow rule for a tool
// whose calls the denylist can block — in practice that is
// shell_exec, whose individual commands are checked per call in
// Gate.Check. (An allow rule for shell_exec would only ever approve
// non-denylisted commands; refusing it outright would make the
// "always allow" button dead for the most common gated tool, so Set
// accepts it and the per-call denylist check in Check remains the
// absolute guard.)
func (s *RuleStore) Set(tool string, action Action) error {
	return s.SetForChat(0, tool, action)
}

// SetForChat stores a rule for tool in chatID and persists it. A
// chatID of 0 stores a global rule (the legacy scope — see
// LookupForChat). An invalid action or an empty tool name is an
// error and stores nothing.
func (s *RuleStore) SetForChat(chatID int64, tool string, action Action) error {
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
	s.rules[ruleKey{chatID: chatID, tool: tool}] = action
	return s.saveLocked()
}

// Delete removes the GLOBAL rule for tool (a no-op when none exists) and
// persists.
func (s *RuleStore) Delete(tool string) error {
	return s.DeleteForChat(0, tool)
}

// DeleteForChat removes the rule for tool in chatID (a no-op when
// none exists) and persists. It removes only that chat's rule: a
// legacy global rule for the same tool, if one exists, is untouched.
func (s *RuleStore) DeleteForChat(chatID int64, tool string) error {
	if s == nil {
		return fmt.Errorf("permissions: rule store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rules, ruleKey{chatID: chatID, tool: strings.TrimSpace(tool)})
	return s.saveLocked()
}

// List returns all rules sorted by chat, then tool name. ChatID 0
// entries are the legacy global rules.
func (s *RuleStore) List() []Rule {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rule, 0, len(s.rules))
	for k, action := range s.rules {
		out = append(out, Rule{ChatID: k.chatID, Tool: k.tool, Action: action})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ChatID != out[j].ChatID {
			return out[i].ChatID < out[j].ChatID
		}
		return out[i].Tool < out[j].Tool
	})
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
	data, err := json.MarshalIndent(persistedRules{Version: 2, Rules: s.encodedRulesLocked()}, "", "  ")
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

// encodedRulesLocked renders the in-memory rules as the on-disk
// map (see encodeRuleKey). Callers must hold s.mu (read or write).
func (s *RuleStore) encodedRulesLocked() map[string]Action {
	out := make(map[string]Action, len(s.rules))
	for k, action := range s.rules {
		out[encodeRuleKey(k)] = action
	}
	return out
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
