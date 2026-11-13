package config

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Telegram allowlist opt-in (ChatGPT rc2 audit, Finding 3) ---

// TestAllowAllTelegramChatsRoundTrip: allow_all_telegram_chats loads
// from JSON and defaults to false when absent.
func TestAllowAllTelegramChatsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA","allow_all_telegram_chats":true}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AllowAllTelegramChats {
		t.Error("AllowAllTelegramChats = false, want true (set in file)")
	}

	// Absent from the file: false, and Save round-trips the true
	// value back to disk.
	path2 := filepath.Join(dir, "config2.json")
	body2 := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA"}`
	if err := os.WriteFile(path2, []byte(body2), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.AllowAllTelegramChats {
		t.Error("AllowAllTelegramChats = true, want false (absent from file)")
	}
	cfg2.AllowAllTelegramChats = true
	if err := cfg2.Save(path2); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg3, err := Load(path2)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if !cfg3.AllowAllTelegramChats {
		t.Error("AllowAllTelegramChats did not survive Save/Load round-trip")
	}
}
