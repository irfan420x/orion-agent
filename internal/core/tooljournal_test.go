package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCanonicalToolArguments(t *testing.T) {
	a := CanonicalToolArguments(`{"b":1,"a":2}`)
	b := CanonicalToolArguments(`{"a":2,"b":1}`)
	if a != b {
		t.Errorf("key order changed the canonical form: %q vs %q", a, b)
	}
	if got := CanonicalToolArguments(`  {"a": 2}  `); got != `{"a":2}` {
		t.Errorf("whitespace: got %q, want %q", got, `{"a":2}`)
	}
	if got := CanonicalToolArguments(``); got != `{}` {
		t.Errorf("empty: got %q, want {}", got)
	}
	if got := CanonicalToolArguments(`not json`); got != `not json` {
		t.Errorf("invalid JSON: got %q, want raw text", got)
	}
	if got := CanonicalToolArguments(`[1,2]`); got != `[1,2]` {
		t.Errorf("non-object: got %q, want raw text", got)
	}
	if got := CanonicalToolArguments(`{"n":1.0}`); got != CanonicalToolArguments(`{"n":1}`) {
		t.Errorf("1.0 vs 1 canonicalise differently: %q vs %q", got, CanonicalToolArguments(`{"n":1}`))
	}
}

func TestToolJournalRecordLookup(t *testing.T) {
	dir := t.TempDir()
	j, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal: %v", err)
	}
	if _, ok := j.Lookup(100, "echo", `{"text":"hi"}`, 0); ok {
		t.Error("Lookup on an empty journal hit, want miss")
	}
	if err := j.Record(100, "echo", `{"text":"hi"}`, "echo:hi", 0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got, ok := j.Lookup(100, "echo", `{"text":"hi"}`, 0); !ok || got != "echo:hi" {
		t.Errorf("Lookup = %q,%v, want \"echo:hi\",true", got, ok)
	}
	// Occurrence, arguments and update id all discriminate.
	if _, ok := j.Lookup(100, "echo", `{"text":"hi"}`, 1); ok {
		t.Error("Lookup occurrence 1 hit, want miss")
	}
	if _, ok := j.Lookup(100, "echo", `{"text":"bye"}`, 0); ok {
		t.Error("Lookup different args hit, want miss")
	}
	if _, ok := j.Lookup(101, "echo", `{"text":"hi"}`, 0); ok {
		t.Error("Lookup different update hit, want miss")
	}
	if _, ok := j.Lookup(100, "other", `{"text":"hi"}`, 0); ok {
		t.Error("Lookup different tool hit, want miss")
	}
	if recs := j.Records(100); len(recs) != 1 {
		t.Errorf("Records(100) = %d, want 1", len(recs))
	}
}

func TestToolJournalPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	j1, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal: %v", err)
	}
	if err := j1.Record(100, "shell_exec", `{"command":"echo hi"}`, "hi\n", 0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// A second instance on the same dir (a restarted daemon)
	// must see run-1's record: the file is the persistence.
	j2, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal (second instance): %v", err)
	}
	if got, ok := j2.Lookup(100, "shell_exec", `{"command":"echo hi"}`, 0); !ok || got != "hi\n" {
		t.Errorf("Lookup after reopen = %q,%v, want \"hi\\n\",true", got, ok)
	}
}

func TestToolJournalPrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	j, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal: %v", err)
	}
	if err := j.Record(100, "echo", `{"text":"hi"}`, "echo:hi", 0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Age the file past the 24h retention, then reopen: the
	// record must be gone (Telegram never re-delivers past 24h).
	path := filepath.Join(dir, "100.jsonl")
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	j2, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal (second instance): %v", err)
	}
	if _, ok := j2.Lookup(100, "echo", `{"text":"hi"}`, 0); ok {
		t.Error("Lookup hit a pruned record, want miss")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pruned file still exists (err=%v)", err)
	}
}

func TestToolJournalBadDir(t *testing.T) {
	if _, err := NewToolJournal(""); err == nil {
		t.Error("NewToolJournal(\"\"): expected error, got nil")
	}
	// A path that exists as a file, not a directory.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := NewToolJournal(f); err == nil {
		t.Error("NewToolJournal(file path): expected error, got nil")
	}
}

func TestUpdateIDContext(t *testing.T) {
	if got := UpdateIDFromContext(nil); got != 0 {
		t.Errorf("UpdateIDFromContext(nil) = %d, want 0", got)
	}
	if got := UpdateIDFromContext(context.Background()); got != 0 {
		t.Errorf("UpdateIDFromContext(background) = %d, want 0", got)
	}
	ctx := WithUpdateID(context.Background(), 100)
	if got := UpdateIDFromContext(ctx); got != 100 {
		t.Errorf("UpdateIDFromContext = %d, want 100", got)
	}
	// Clearing the id (subagent child turns) reads back as 0.
	cleared := WithUpdateID(ctx, 0)
	if got := UpdateIDFromContext(cleared); got != 0 {
		t.Errorf("UpdateIDFromContext(cleared) = %d, want 0", got)
	}
	if got := UpdateIDFromContext(WithUpdateID(nil, 7)); got != 7 {
		t.Errorf("UpdateIDFromContext(nil ctx) = %d, want 7", got)
	}
}
