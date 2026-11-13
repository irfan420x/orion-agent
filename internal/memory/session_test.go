package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionLogAppendRead(t *testing.T) {
	dir := t.TempDir()
	l, err := NewSessionLog(dir)
	if err != nil {
		t.Fatalf("NewSessionLog: %v", err)
	}
	// Empty log reads as "".
	if got, err := l.Read(); err != nil || got != "" {
		t.Fatalf("Read empty: got %q err %v", got, err)
	}
	if err := l.Append("Decided to use Go for the core."); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Append("Irfan confirmed the Telegram-only rule."); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := l.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(got, "Decided to use Go for the core.") {
		t.Errorf("log missing first entry:\n%s", got)
	}
	if !strings.Contains(got, "Irfan confirmed the Telegram-only rule.") {
		t.Errorf("log missing second entry:\n%s", got)
	}
	if count := strings.Count(got, "## "); count != 2 {
		t.Errorf("log has %d timestamp headers, want 2:\n%s", count, got)
	}
	if !strings.Contains(got, "UTC") {
		t.Errorf("entries should carry a UTC timestamp:\n%s", got)
	}
	// File mode is 0600.
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode: got %o want 0600", info.Mode().Perm())
	}
}

func TestSessionLogAppendEmpty(t *testing.T) {
	l, err := NewSessionLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionLog: %v", err)
	}
	if err := l.Append("   "); err == nil {
		t.Error("Append(empty): expected an error, got none")
	}
	if err := l.Append(""); err == nil {
		t.Error("Append(\"\"): expected an error, got none")
	}
}

func TestSessionLogCap(t *testing.T) {
	dir := t.TempDir()
	l, err := NewSessionLog(dir)
	if err != nil {
		t.Fatalf("NewSessionLog: %v", err)
	}
	// Append enough entries to blow past the 64 KiB cap several times
	// over. Each entry is ~1 KiB.
	big := strings.Repeat("x", 1000)
	for i := 0; i < 200; i++ {
		if err := l.Append(fmt.Sprintf("entry %03d %s", i, big)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	got, err := l.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) > MaxSessionMemoryBytes {
		t.Errorf("log size %d exceeds cap %d", len(got), MaxSessionMemoryBytes)
	}
	// The newest entries survive; the oldest are trimmed.
	if !strings.Contains(got, "entry 199") {
		t.Error("log lost the newest entry")
	}
	if strings.Contains(got, "entry 000") {
		t.Error("log still contains the oldest entry; trimming did not drop it")
	}
	if strings.Count(got, "## ") < 10 {
		t.Errorf("log kept too few entries (%d headers)", strings.Count(got, "## "))
	}
	// The file is still well-formed: every entry has a header.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "## ") && !strings.Contains(line, "UTC") {
			t.Errorf("malformed entry header: %q", line)
		}
	}
}

func TestSessionLogSingleHugeEntry(t *testing.T) {
	l, err := NewSessionLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionLog: %v", err)
	}
	huge := strings.Repeat("y", MaxSessionMemoryBytes*2)
	if err := l.Append(huge); err != nil {
		t.Fatalf("Append huge: %v", err)
	}
	got, err := l.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) > MaxSessionMemoryBytes {
		t.Errorf("single huge entry: size %d exceeds cap %d", len(got), MaxSessionMemoryBytes)
	}
	if !strings.HasPrefix(got, "## ") {
		t.Errorf("truncated entry lost its header: %q", got[:min(40, len(got))])
	}
}

func TestSessionLogClear(t *testing.T) {
	l, err := NewSessionLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionLog: %v", err)
	}
	if err := l.Append("something"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got, err := l.Read(); err != nil || got != "" {
		t.Fatalf("Read after clear: got %q err %v", got, err)
	}
	if _, err := os.Stat(l.Path()); !os.IsNotExist(err) {
		t.Fatalf("file should be gone after Clear, stat err %v", err)
	}
	// Appending after a clear works.
	if err := l.Append("fresh entry"); err != nil {
		t.Fatalf("Append after clear: %v", err)
	}
}

func TestSessionLogPersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	l1, err := NewSessionLog(dir)
	if err != nil {
		t.Fatalf("NewSessionLog: %v", err)
	}
	if err := l1.Append("first session entry"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	l2, err := NewSessionLog(dir)
	if err != nil {
		t.Fatalf("NewSessionLog again: %v", err)
	}
	got, err := l2.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(got, "first session entry") {
		t.Errorf("reopened log lost the entry:\n%s", got)
	}
	// The log lives next to memory.json, not inside memories/.
	if filepath.Dir(l1.Path()) != dir {
		t.Errorf("session log dir: got %q want %q", filepath.Dir(l1.Path()), dir)
	}
}
