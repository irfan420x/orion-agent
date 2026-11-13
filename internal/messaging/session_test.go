package messaging

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Session persistence (ChatGPT rc2 audit, Finding 6) ---
//
// saveLocked writes atomically (temp file + fsync + rename) and then
// fsyncs the parent directory, so the rename itself is durable — the
// same pattern the memory and config stores use. These tests pin the
// persistence contract the fsync protects: a saved session must
// survive a fresh load, and no temp file may be left behind.

// TestSessionStoreRoundTripPersists: titles, footer preference,
// usage totals and the pause flag survive a reload from disk.
func TestSessionStoreRoundTripPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	store := NewSessionStore(path, true)
	if err := store.SetTitle(42, "my chat"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if err := store.SetFooter(42, false); err != nil {
		t.Fatalf("SetFooter: %v", err)
	}
	if err := store.AddUsage(42, 150); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	if err := store.AddUsage(7, 25); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	if err := store.SetPaused(true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}

	reloaded := NewSessionStore(path, true)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload LoadError: %v", err)
	}
	sess := reloaded.Get(42)
	if sess.Title != "my chat" {
		t.Errorf("reloaded title = %q, want %q", sess.Title, "my chat")
	}
	if sess.FooterEnabled {
		t.Error("reloaded footer = on, want off")
	}
	if sess.TotalTokens != 150 || sess.Turns != 1 {
		t.Errorf("reloaded usage = %d tokens/%d turns, want 150/1", sess.TotalTokens, sess.Turns)
	}
	if got := reloaded.Get(7); got.TotalTokens != 25 {
		t.Errorf("reloaded chat 7 tokens = %d, want 25", got.TotalTokens)
	}
	if !reloaded.IsPaused() {
		t.Error("reloaded pause flag = false, want true")
	}
	// A chat never seen before still gets the footer default.
	if got := reloaded.Get(99); !got.FooterEnabled {
		t.Error("new chat footer = off, want the store default (on)")
	}
}

// TestSessionStoreLeavesNoTempFiles: every save renames its temp file
// away, so the directory must hold only sessions.json afterwards.
func TestSessionStoreLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	store := NewSessionStore(path, true)
	if err := store.SetTitle(1, "t"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if err := store.SetPaused(false); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sessions.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("sessions dir = %v, want only [sessions.json]", names)
	}
}
