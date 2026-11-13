package config

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Audit 2: atomic Save --------------------------------------------------

// TestSaveEnforces0600OverLaxFile: Save must replace a pre-existing
// config file (even one created with laxer permissions) with a 0600
// one — a plain os.WriteFile never changes the mode of an existing
// file, so a 0644 config.json would stay world-readable, leaking the
// API keys and Telegram token it holds. The saved content must still
// load and round-trip.
func TestSaveEnforces0600OverLaxFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("ORION_PROVIDER", "")
	t.Setenv("ORION_API_KEY", "")
	t.Setenv("ORION_MODEL", "")
	t.Setenv("ORION_TELEGRAM_TOKEN", "")
	t.Setenv("ORION_DATA_DIR", "")
	cfg := Default()
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("saved config mode = %o, want 0600", got)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load(saved config): %v", err)
	}
	if loaded.DefaultProvider != "ollama" {
		t.Errorf("loaded DefaultProvider = %q, want ollama", loaded.DefaultProvider)
	}
}

// TestSaveCreates0600File: a fresh Save creates the file with 0600.
func TestSaveCreates0600File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.json")
	t.Setenv("ORION_PROVIDER", "")
	t.Setenv("ORION_API_KEY", "")
	t.Setenv("ORION_MODEL", "")
	t.Setenv("ORION_TELEGRAM_TOKEN", "")
	t.Setenv("ORION_DATA_DIR", "")
	cfg := Default()
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("saved config mode = %o, want 0600", got)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load(saved config): %v", err)
	}
}
