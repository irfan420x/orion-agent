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
// usage totals and the per-chat pause flag survive a reload from disk.
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
	if err := store.SetPaused(42, true); err != nil {
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
	if !reloaded.IsPaused(42) {
		t.Error("reloaded pause flag for chat 42 = false, want true")
	}
	// Pausing one chat must not pause another.
	if reloaded.IsPaused(7) {
		t.Error("reloaded pause flag for chat 7 = true, want false (pause is per chat)")
	}
	// A chat never seen before still gets the footer default.
	if got := reloaded.Get(99); !got.FooterEnabled {
		t.Error("new chat footer = off, want the store default (on)")
	}
}

// TestSessionStorePauseIsPerChat: pausing chat A leaves chat B
// running, and unpausing A does not touch B.
func TestSessionStorePauseIsPerChat(t *testing.T) {
	dir := t.TempDir()
	store := NewSessionStore(filepath.Join(dir, "sessions.json"), true)
	if err := store.SetPaused(42, true); err != nil {
		t.Fatal(err)
	}
	if !store.IsPaused(42) {
		t.Error("chat 42 not paused after SetPaused(true)")
	}
	if store.IsPaused(7) {
		t.Error("chat 7 paused by chat 42's /pause")
	}
	if err := store.SetPaused(42, false); err != nil {
		t.Fatal(err)
	}
	if store.IsPaused(42) {
		t.Error("chat 42 still paused after SetPaused(false)")
	}
}

// TestSessionStoreMigratesLegacyGlobalPause: a sessions file written
// before P1-O3 (single daemon-wide "paused" flag) loads with every
// stored session paused, and the flag is dropped on the next save.
func TestSessionStoreMigratesLegacyGlobalPause(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	legacy := `{"version":1,"paused":true,"sessions":{"42":{"chat_id":42,"footer_enabled":true},"7":{"chat_id":7,"footer_enabled":true}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(path, true)
	if err := store.LoadError(); err != nil {
		t.Fatalf("LoadError: %v", err)
	}
	if !store.IsPaused(42) || !store.IsPaused(7) {
		t.Error("legacy global pause did not migrate onto the stored sessions")
	}
	// Next save drops the legacy flag (a fresh load then shows the
	// per-chat flags, not a global one).
	if err := store.SetTitle(42, "x"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stringContains(string(data), `"paused":true`) {
		t.Errorf("saved file still carries the legacy global paused flag: %s", data)
	}
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
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
	if err := store.SetPaused(1, false); err != nil {
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
