// Package memory — typed memory files (Memory v2).
//
// Typed memories are markdown files under <dataDir>/memories/*.md with
// YAML-style frontmatter:
//
//	---
//	name: Favourite editor
//	description: Irfan prefers Zed over VS Code for Go work
//	type: user
//	---
//	<body>
//
// The four types come from the MEMORY_V2_PLAN (Claude Code-inspired,
// patterns only, re-implemented in Go):
//   - user      — who the user is: role, goals, knowledge, preferences.
//   - feedback  — guidance on how to work: corrections AND confirmations.
//     Body structure is enforced: rule first, then "Why:"
//     and "How to apply:" lines (see feedback.go).
//   - project   — ongoing work, goals, bugs, incidents. Relative dates
//     in the body are converted to absolute dates on save
//     (see dates.go).
//   - reference — pointers to external systems (a Linear project, a
//     Grafana board, a Slack channel).
//
// File names are slugified from the memory name. Writes are atomic
// (temp file in the same directory + fsync + rename + directory fsync,
// mode 0600, directory 0700), exactly like the JSON store in
// file_store.go, so a crash mid-write never leaves a torn file behind.
//
// The JSON store (FileStore / memory.json) stays authoritative for the
// Store interface. Typed memories are a separate layer on top of the
// same data directory; migration (migrate.go) copies JSON facts into
// typed files without touching the JSON store.
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// MemoryType is one of the four typed-memory types.
type MemoryType string

const (
	// TypeUser is a memory about who the user is.
	TypeUser MemoryType = "user"
	// TypeFeedback is guidance on how to work (correction or confirmation).
	TypeFeedback MemoryType = "feedback"
	// TypeProject is about ongoing work, goals, bugs, incidents.
	TypeProject MemoryType = "project"
	// TypeReference points at an external system.
	TypeReference MemoryType = "reference"
)

// AllMemoryTypes lists the valid types, in the order the plan defines them.
var AllMemoryTypes = []MemoryType{TypeUser, TypeFeedback, TypeProject, TypeReference}

// ValidMemoryType reports whether t is one of the four known types.
func ValidMemoryType(t MemoryType) bool {
	switch t {
	case TypeUser, TypeFeedback, TypeProject, TypeReference:
		return true
	}
	return false
}

// ParseMemoryType parses a type string, returning an error for anything
// unknown. A missing or unknown type is an error, never silently
// defaulted — a mistyped frontmatter value must be caught, not guessed.
func ParseMemoryType(s string) (MemoryType, error) {
	t := MemoryType(strings.TrimSpace(strings.ToLower(s)))
	if !ValidMemoryType(t) {
		return "", fmt.Errorf("memory: unknown memory type %q (want one of: user, feedback, project, reference)", s)
	}
	return t, nil
}

var (
	// ErrEmptyMemoryName is returned when a typed memory has no name.
	ErrEmptyMemoryName = errors.New("memory: typed memory name must not be empty")
	// ErrEmptyMemoryDescription is returned when a typed memory has no description.
	ErrEmptyMemoryDescription = errors.New("memory: typed memory description must not be empty")
	// ErrEmptyMemoryBody is returned when a typed memory has an empty body.
	ErrEmptyMemoryBody = errors.New("memory: typed memory body must not be empty")
)

// MemoryProvenance records where a typed memory came from. It is
// the prompt-injection defence (Audit 4 A P1-3): text the model
// produced must never be promoted to standing guidance, so every
// memory carries its origin and recall treats non-user provenance
// as data, never as an instruction.
//
//   - ProvenanceUser — written by the user (or by an explicit
//     user-confirmed save, e.g. NewFeedbackMemory). Only these may
//     become feedback memories.
//   - ProvenanceMigrated — copied from the JSON facts by
//     MigrateFacts. Always type project, never feedback.
//   - ProvenanceModelDerived — produced by the model (session
//     extraction, /compress summaries). Never feedback; rendered
//     as data.
//   - "" (empty) — legacy files written before provenance existed;
//     treated as user-written for rendering (grandfathered), but
//     TypedStore.Save refuses to CREATE a feedback memory with an
//     empty provenance through any path except NewFeedbackMemory,
//     which always sets it.
type MemoryProvenance string

const (
	ProvenanceUser         MemoryProvenance = "user"
	ProvenanceMigrated     MemoryProvenance = "migrated"
	ProvenanceModelDerived MemoryProvenance = "model-derived"
)

// ValidMemoryProvenance reports whether p is a known provenance
// (empty counts as valid: legacy files predate the field).
func ValidMemoryProvenance(p MemoryProvenance) bool {
	switch p {
	case "", ProvenanceUser, ProvenanceMigrated, ProvenanceModelDerived:
		return true
	}
	return false
}

// ParseMemoryProvenance parses a provenance string; anything
// unknown is an error, never silently defaulted.
func ParseMemoryProvenance(s string) (MemoryProvenance, error) {
	p := MemoryProvenance(strings.TrimSpace(strings.ToLower(s)))
	if !ValidMemoryProvenance(p) {
		return "", fmt.Errorf("memory: unknown memory provenance %q (want one of: user, migrated, model-derived)", s)
	}
	return p, nil
}

// TypedMemory is one typed memory file: frontmatter plus a body.
type TypedMemory struct {
	// Name is the human-readable name; it is slugified for the file name.
	Name string
	// Description is what recall matches on. Keep it a short summary
	// of what the memory is about.
	Description string
	// Type is one of the four MemoryTypes.
	Type MemoryType
	// Body is the markdown body below the frontmatter.
	Body string
	// Provenance records where the memory came from (user-written,
	// migrated from JSON facts, or model-derived). See
	// MemoryProvenance: only user provenance may be feedback, and
	// recall renders non-user provenance as data, never as an
	// instruction.
	Provenance MemoryProvenance
	// UpdatedAt is the file's modification time when loaded from disk
	// (zero when the memory was constructed in memory and not saved).
	UpdatedAt time.Time
	// Stale marks a memory whose references drift verification proved
	// no longer exist in the current tree. Stale memories are never
	// re-surfaced by recall, but they are kept (and listed) until the
	// user forgets them, so the staleness is visible, not silent.
	Stale bool
}

// Validate checks the memory's structure: name, description and body
// non-empty, type known, and — for feedback memories — the rule / Why: /
// How to apply: structure (see ValidateFeedback).
func (m TypedMemory) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return ErrEmptyMemoryName
	}
	if strings.TrimSpace(m.Description) == "" {
		return ErrEmptyMemoryDescription
	}
	if !ValidMemoryType(m.Type) {
		return fmt.Errorf("memory: unknown memory type %q (want one of: user, feedback, project, reference)", string(m.Type))
	}
	if strings.TrimSpace(m.Body) == "" {
		return ErrEmptyMemoryBody
	}
	if m.Type == TypeFeedback {
		if err := ValidateFeedback(m); err != nil {
			return err
		}
	}
	return nil
}

// Slugify converts a memory name into a safe file-name stem: lowercase,
// runs of non-alphanumeric characters become a single hyphen, leading
// and trailing hyphens trimmed. A name with no alphanumeric characters
// at all becomes "memory". The result is capped at 100 characters so
// file names stay sane.
func Slugify(name string) string {
	var b strings.Builder
	lastHyphen := true // treat the start as a hyphen so leading junk is dropped
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteRune('-')
				lastHyphen = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "memory"
	}
	if len(s) > 100 {
		s = strings.Trim(s[:100], "-")
		if s == "" {
			return "memory"
		}
	}
	return s
}

// memoriesDirName is the subdirectory of the data dir holding typed files.
const memoriesDirName = "memories"

// MaxMemoryFiles is the manifest cap from the v2 plan: recall scans at
// most this many headers, newest first.
const MaxMemoryFiles = 200

// MemoryHeader is the recall manifest entry for one typed memory:
// everything needed to decide relevance without reading the body.
type MemoryHeader struct {
	Name        string
	Description string
	Type        MemoryType
	// FileName is the file name inside the memories directory.
	FileName string
	// UpdatedAt is the file's modification time.
	UpdatedAt time.Time
}

// TypedStore is the typed-memory layer over a data directory. It is
// safe for concurrent use.
type TypedStore struct {
	mu          sync.RWMutex
	dataDir     string
	memoriesDir string
}

// TypedStoreOption configures a TypedStore at open time.
type TypedStoreOption func(*typedStoreOptions)

type typedStoreOptions struct {
	// migrateFrom, when set, runs MigrateFacts against that JSON store
	// at open time. Migration is opt-in only: NewTypedStore never
	// migrates unless this option is given, so existing callers see
	// no behaviour change.
	migrateFrom *FileStore
}

// WithMigrateOnOpen opts a TypedStore into running MigrateFacts at
// open time against the given JSON store. The JSON facts are copied,
// never modified or deleted. If migration fails, NewTypedStore fails.
func WithMigrateOnOpen(store *FileStore) TypedStoreOption {
	return func(o *typedStoreOptions) { o.migrateFrom = store }
}

// NewTypedStore opens (or creates) the typed-memory layer rooted at
// dataDir: it creates <dataDir>/memories (0700) when missing. It does
// not read or validate existing files at open time — a corrupt file is
// reported by the call that touches it, not by opening the store —
// and it never migrates JSON facts unless WithMigrateOnOpen is given.
func NewTypedStore(dataDir string, opts ...TypedStoreOption) (*TypedStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("memory: data directory must not be empty")
	}
	var o typedStoreOptions
	for _, opt := range opts {
		opt(&o)
	}
	memoriesDir := filepath.Join(dataDir, memoriesDirName)
	// Only a directory this call creates gets its mode enforced
	// (0700); a pre-existing directory is left exactly as it is
	// (same rule as NewFileStore).
	_, statErr := os.Stat(memoriesDir)
	created := errors.Is(statErr, os.ErrNotExist)
	if err := os.MkdirAll(memoriesDir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create memories dir %q: %w", memoriesDir, err)
	}
	if created {
		// MkdirAll applies the mode subject to umask, so enforce
		// 0700 exactly.
		if err := os.Chmod(memoriesDir, 0o700); err != nil {
			return nil, fmt.Errorf("memory: chmod memories dir %q: %w", memoriesDir, err)
		}
	}
	ts := &TypedStore{dataDir: dataDir, memoriesDir: memoriesDir}
	if o.migrateFrom != nil {
		if _, err := ts.MigrateFacts(o.migrateFrom); err != nil {
			return nil, err
		}
	}
	return ts, nil
}

// TypedStore returns the typed-memory layer over this FileStore's data
// directory. It is a convenience for the coordinator wiring the two
// layers together; it does not migrate anything.
func (s *FileStore) TypedStore() (*TypedStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkOpenLocked(); err != nil {
		return nil, err
	}
	return NewTypedStore(s.dataDir)
}

// MemoriesDir returns the directory holding the typed memory files.
func (s *TypedStore) MemoriesDir() string { return s.memoriesDir }

// DataDir returns the data directory this store is rooted at.
func (s *TypedStore) DataDir() string { return s.dataDir }

// Save validates and writes a typed memory, atomically. The file name
// is the slug of the memory name plus ".md". Saving a memory whose
// slug already exists overwrites that file (same slug = same memory).
// Project memories have relative dates in the body converted to
// absolute dates relative to the save time before writing.
//
// Fail-closed provenance (Audit 4 A P1-3): a feedback memory may
// only be saved with user provenance (written by the user, or built
// by NewFeedbackMemory from an explicit user confirmation). A
// migrated or model-derived feedback memory is refused outright —
// model-derived text must never become standing guidance, however
// it reached the store.
func (s *TypedStore) Save(m TypedMemory) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Type == TypeFeedback && m.Provenance != "" && m.Provenance != ProvenanceUser {
		return fmt.Errorf("memory: feedback memory %q has provenance %q; only user-written feedback may be saved (model-derived or migrated text must never become standing guidance)", m.Name, string(m.Provenance))
	}
	if m.Type == TypeProject {
		m.Body = ConvertRelativeDates(m.Body, time.Now())
	}
	data, err := MarshalTypedMemory(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeFileLocked(Slugify(m.Name)+".md", data)
}

// Load reads a typed memory by name (slugified to the file name). It
// reports found=false when no file exists for that name.
func (s *TypedStore) Load(name string) (TypedMemory, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(filepath.Join(s.memoriesDir, Slugify(name)+".md"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TypedMemory{}, false, nil
		}
		return TypedMemory{}, false, fmt.Errorf("memory: read typed memory %q: %w", name, err)
	}
	m, err := ParseTypedMemory(data)
	if err != nil {
		return TypedMemory{}, false, fmt.Errorf("memory: parse typed memory %q: %w", name, err)
	}
	if info, err := os.Stat(filepath.Join(s.memoriesDir, Slugify(name)+".md")); err == nil {
		m.UpdatedAt = info.ModTime().UTC()
	}
	return m, true, nil
}

// Delete removes a typed memory by name. Deleting a memory that does
// not exist is a no-op, not an error.
func (s *TypedStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(filepath.Join(s.memoriesDir, Slugify(name)+".md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory: delete typed memory %q: %w", name, err)
	}
	return nil
}

// MarkStale records that a typed memory's references no longer exist
// in the current tree (drift verification). The memory is kept, with
// `stale: true` in its frontmatter, so the staleness is visible in
// listings until the user forgets it; recall never re-surfaces it.
// Marking a memory that does not exist is a no-op, not an error.
func (s *TypedStore) MarkStale(name string) error {
	m, found, err := s.Load(name)
	if err != nil {
		return err
	}
	if !found || m.Stale {
		return nil
	}
	m.Stale = true
	return s.Save(m)
}

// List returns all typed memories sorted by name. A corrupt file is
// an error, not silently skipped — a bad memory file must be noticed,
// not hidden.
func (s *TypedStore) List() ([]TypedMemory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := s.listEntriesLocked()
	if err != nil {
		return nil, err
	}
	out := make([]TypedMemory, 0, len(entries))
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(s.memoriesDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("memory: read typed memory %q: %w", e.Name(), err)
		}
		m, err := ParseTypedMemory(data)
		if err != nil {
			return nil, fmt.Errorf("memory: parse typed memory %q: %w", e.Name(), err)
		}
		if info, err := e.Info(); err == nil {
			m.UpdatedAt = info.ModTime().UTC()
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Headers returns the recall manifest: one header per typed memory
// (name, description, type, file, mtime), newest first, capped at
// MaxMemoryFiles. This is what recall v2 scans before selecting
// memories to load — bodies are only read for the selected few.
func (s *TypedStore) Headers() ([]MemoryHeader, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := s.listEntriesLocked()
	if err != nil {
		return nil, err
	}
	// Newest first.
	type entryInfo struct {
		entry os.DirEntry
		info  os.FileInfo
	}
	pairs := make([]entryInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("memory: stat typed memory %q: %w", e.Name(), err)
		}
		pairs = append(pairs, entryInfo{entry: e, info: info})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].info.ModTime().After(pairs[j].info.ModTime())
	})
	if len(pairs) > MaxMemoryFiles {
		pairs = pairs[:MaxMemoryFiles]
	}
	out := make([]MemoryHeader, 0, len(pairs))
	for _, p := range pairs {
		e := p.entry
		data, err := os.ReadFile(filepath.Join(s.memoriesDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("memory: read typed memory %q: %w", e.Name(), err)
		}
		m, err := ParseTypedMemory(data)
		if err != nil {
			return nil, fmt.Errorf("memory: parse typed memory %q: %w", e.Name(), err)
		}
		out = append(out, MemoryHeader{
			Name:        m.Name,
			Description: m.Description,
			Type:        m.Type,
			FileName:    e.Name(),
			UpdatedAt:   p.info.ModTime().UTC(),
		})
	}
	return out, nil
}

// listEntriesLocked returns the *.md files in the memories directory.
// Callers must hold s.mu (read or write).
func (s *TypedStore) listEntriesLocked() ([]os.DirEntry, error) {
	entries, err := os.ReadDir(s.memoriesDir)
	if err != nil {
		return nil, fmt.Errorf("memory: list memories dir %q: %w", s.memoriesDir, err)
	}
	out := entries[:0]
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// writeFileLocked atomically writes data as fileName inside the
// memories directory: temp file (0600) + fsync + rename + directory
// fsync, mirroring FileStore.saveLocked. Callers must hold s.mu (write).
func (s *TypedStore) writeFileLocked(fileName string, data []byte) error {
	tmp, err := os.CreateTemp(s.memoriesDir, ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temp file: %w", err)
	}
	tmpName := tmp.Name()
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
	if err := os.Rename(tmpName, filepath.Join(s.memoriesDir, fileName)); err != nil {
		return fmt.Errorf("memory: rename temp file to %q: %w", fileName, err)
	}
	if err := syncDir(s.memoriesDir); err != nil {
		return fmt.Errorf("memory: sync memories dir %q: %w", s.memoriesDir, err)
	}
	return nil
}

// ParseTypedMemory parses a typed memory file's contents: a
// frontmatter block delimited by "---" lines, then the body. The
// frontmatter must contain name, description and type; type must be
// one of the four known types. The body must be non-empty.
func ParseTypedMemory(data []byte) (TypedMemory, error) {
	text := string(data)
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return TypedMemory{}, errors.New("memory: typed memory must start with a frontmatter block delimited by ---")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return TypedMemory{}, errors.New("memory: frontmatter block is not closed with ---")
	}
	fields := make(map[string]string)
	for _, line := range lines[1:end] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return TypedMemory{}, fmt.Errorf("memory: malformed frontmatter line %q (want key: value)", line)
		}
		key = strings.TrimSpace(strings.ToLower(key))
		if key == "" {
			return TypedMemory{}, fmt.Errorf("memory: malformed frontmatter line %q (empty key)", line)
		}
		fields[key] = unquote(strings.TrimSpace(value))
	}
	var m TypedMemory
	var ok bool
	if m.Name, ok = fields["name"]; !ok || strings.TrimSpace(m.Name) == "" {
		return TypedMemory{}, errors.New("memory: frontmatter is missing a non-empty name")
	}
	if m.Description, ok = fields["description"]; !ok || strings.TrimSpace(m.Description) == "" {
		return TypedMemory{}, errors.New("memory: frontmatter is missing a non-empty description")
	}
	typeStr, ok := fields["type"]
	if !ok || strings.TrimSpace(typeStr) == "" {
		return TypedMemory{}, errors.New("memory: frontmatter is missing a non-empty type")
	}
	t, err := ParseMemoryType(typeStr)
	if err != nil {
		return TypedMemory{}, err
	}
	m.Type = t
	if v, ok := fields["stale"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			m.Stale = true
		case "false":
			// Explicitly not stale; the zero value already says so.
		default:
			return TypedMemory{}, fmt.Errorf("memory: frontmatter stale value %q is not true or false", v)
		}
	}
	if v, ok := fields["provenance"]; ok {
		p, err := ParseMemoryProvenance(v)
		if err != nil {
			return TypedMemory{}, err
		}
		m.Provenance = p
	}
	body := strings.Trim(strings.Join(lines[end+1:], "\n"), "\n")
	// Keep the body's internal blank lines; only the surrounding
	// newlines are trimmed. A body that is only whitespace is empty.
	if strings.TrimSpace(body) == "" {
		return TypedMemory{}, ErrEmptyMemoryBody
	}
	m.Body = body
	return m, nil
}

// MarshalTypedMemory renders a memory as file contents: frontmatter
// followed by the body. The result round-trips through
// ParseTypedMemory.
func MarshalTypedMemory(m TypedMemory) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", quoteValue(strings.TrimSpace(m.Name)))
	fmt.Fprintf(&b, "description: %s\n", quoteValue(strings.TrimSpace(m.Description)))
	fmt.Fprintf(&b, "type: %s\n", string(m.Type))
	if m.Provenance != "" {
		fmt.Fprintf(&b, "provenance: %s\n", string(m.Provenance))
	}
	if m.Stale {
		b.WriteString("stale: true\n")
	}
	b.WriteString("---\n")
	b.WriteString(strings.Trim(m.Body, "\n"))
	b.WriteString("\n")
	return []byte(b.String()), nil
}

// quoteValue quotes a frontmatter value when it contains characters
// that would confuse the simple key: value parser (a colon, a leading
// or trailing space, or a '#'), escaping backslashes and double
// quotes. Plain values stay unquoted for readability.
func quoteValue(v string) string {
	if strings.ContainsAny(v, ":#") || strings.HasPrefix(v, " ") || strings.HasSuffix(v, " ") || strings.Contains(v, "\"") {
		return "\"" + strings.ReplaceAll(strings.ReplaceAll(v, "\\", "\\\\"), "\"", "\\\"") + "\""
	}
	return v
}

// unquote removes one layer of matching single or double quotes from a
// frontmatter value, unescaping \" and \\ inside double quotes.
func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		inner = strings.ReplaceAll(inner, "\\\"", "\"")
		inner = strings.ReplaceAll(inner, "\\\\", "\\")
		return inner
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}
