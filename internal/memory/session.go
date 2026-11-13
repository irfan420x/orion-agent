// Package memory — session memory file (Memory v2).
//
// Session memory is the append-only log at <dataDir>/session-memory.md
// that workstream B's background extractor writes to and workstream
// C's /compress reads from, so the important bits of a conversation
// (decisions, corrections, facts worth keeping) survive context
// compression. Entries are timestamped markdown sections:
//
//	## 2026-11-13 11:45:02 UTC
//	<entry>
//
// Writes are atomic (temp file + fsync + rename + dir fsync, 0600)
// and the file is capped at MaxSessionMemoryBytes: when an append
// would exceed the cap, the oldest entries are trimmed first, so the
// file stays bounded without losing the newest entries.
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxSessionMemoryBytes is the cap on the session-memory file (64 KiB).
const MaxSessionMemoryBytes = 64 * 1024

// sessionMemoryFileName is the session-memory file inside the data dir.
const sessionMemoryFileName = "session-memory.md"

// sessionEntryHeaderPrefix marks the start of one entry.
const sessionEntryHeaderPrefix = "## "

// SessionLog is the append-only session-memory log for one data
// directory. It is safe for concurrent use.
type SessionLog struct {
	mu   sync.Mutex
	path string
}

// NewSessionLog opens the session-memory log rooted at dataDir. It
// creates the data directory (0700) when missing; it does not create
// the file until the first append.
func NewSessionLog(dataDir string) (*SessionLog, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("memory: data directory must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create data dir %q: %w", dataDir, err)
	}
	return &SessionLog{path: filepath.Join(dataDir, sessionMemoryFileName)}, nil
}

// Path returns the session-memory file path.
func (l *SessionLog) Path() string { return l.path }

// Append adds one entry to the log with a UTC timestamp header. An
// empty entry is an error. If the file would exceed
// MaxSessionMemoryBytes, the oldest entries are trimmed first (the
// newest entries are always kept); a single entry larger than the cap
// is truncated to fit, keeping its header.
func (l *SessionLog) Append(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return errors.New("memory: session-memory entry must not be empty")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	var existing string
	data, err := os.ReadFile(l.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory: read session-memory %q: %w", l.path, err)
	}
	if err == nil {
		existing = string(data)
	}
	header := sessionEntryHeaderPrefix + time.Now().UTC().Format("2006-01-02 15:04:05 UTC") + "\n"
	block := header + entry + "\n"
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	combined := existing + "\n" + block
	combined = strings.TrimLeft(combined, "\n")
	combined = trimSessionLog(combined, MaxSessionMemoryBytes)
	return l.writeLocked([]byte(combined))
}

// Read returns the whole session-memory log, or "" when the file does
// not exist yet.
func (l *SessionLog) Read() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := os.ReadFile(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("memory: read session-memory %q: %w", l.path, err)
	}
	return string(data), nil
}

// Clear removes all entries from the log. The file itself is removed;
// the next append recreates it.
func (l *SessionLog) Clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory: clear session-memory %q: %w", l.path, err)
	}
	return nil
}

// writeLocked atomically writes data to the session-memory file.
// Callers must hold l.mu.
func (l *SessionLog) writeLocked(data []byte) error {
	dir := filepath.Dir(l.path)
	tmp, err := os.CreateTemp(dir, ".session-memory-*.tmp")
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
	if err := os.Rename(tmpName, l.path); err != nil {
		return fmt.Errorf("memory: rename temp file to %q: %w", l.path, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("memory: sync data dir %q: %w", dir, err)
	}
	return nil
}

// trimSessionLog drops the oldest whole entries from content until it
// fits within maxBytes. Entries are "## <timestamp>" sections. A
// single entry larger than maxBytes is truncated to fit, keeping its
// header line and as much of its body as fits.
func trimSessionLog(content string, maxBytes int) string {
	if len(content) <= maxBytes {
		return content
	}
	entries := splitSessionEntries(content)
	for len(entries) > 1 && totalLen(entries) > maxBytes {
		entries = entries[1:]
	}
	if len(entries) == 1 && len(entries[0]) > maxBytes {
		// Truncate the single remaining entry, keeping its header.
		lines := strings.SplitN(entries[0], "\n", 2)
		header := lines[0] + "\n"
		rest := ""
		if len(lines) > 1 {
			rest = lines[1]
		}
		room := maxBytes - len(header)
		if room < 0 {
			room = 0
		}
		if len(rest) > room {
			rest = rest[:room]
		}
		return header + rest
	}
	return strings.Join(entries, "")
}

// splitSessionEntries splits content into its "## " sections. Text
// before the first header (if any) is kept attached to the first
// entry, so nothing is silently dropped.
func splitSessionEntries(content string) []string {
	lines := strings.SplitAfter(content, "\n")
	var entries []string
	var cur strings.Builder
	for _, line := range lines {
		if strings.HasPrefix(line, sessionEntryHeaderPrefix) && cur.Len() > 0 {
			entries = append(entries, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		entries = append(entries, cur.String())
	}
	if len(entries) == 0 {
		return []string{content}
	}
	return entries
}

func totalLen(entries []string) int {
	n := 0
	for _, e := range entries {
		n += len(e)
	}
	return n
}
