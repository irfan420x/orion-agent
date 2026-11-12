package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Hardening tests (audit P1-6, P1-7; P0-2 allowlist validation) ---

// TestValidateTelegramAllowedChats: a zero chat id is never valid
// (private chats are positive, groups negative, zero is neither).
func TestValidateTelegramAllowedChats(t *testing.T) {
	cfg := Default()
	cfg.TelegramAllowedChats = []int64{123, 0, -456}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "telegram_allowed_chats") {
		t.Fatalf("zero chat id: err=%v, want telegram_allowed_chats error", err)
	}
	cfg.TelegramAllowedChats = []int64{123, -456}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid allowlist: %v", err)
	}
}

// TestSaveDoesNotPersistEnvSecrets: an API key or Telegram token
// supplied by the environment must never be written to the config file
// by Save — otherwise a later `orion config set-provider` would leak
// env secrets to disk.
func TestSaveDoesNotPersistEnvSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	fileKey := "file-key-12345678"
	fileToken := "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	cfgJSON := `{
		"default_provider": "openai",
		"providers": {"openai": {"name": "openai", "model": "gpt-4o-mini", "api_key": "` + fileKey + `"}},
		"telegram_token": "` + fileToken + `",
		"data_dir": "` + dir + `",
		"max_steps": 20,
		"timeout_seconds": 120
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("ORION_API_KEY", "env-key-87654321")
	t.Setenv("ORION_TELEGRAM_TOKEN", "987654321:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Providers["openai"].APIKey != "env-key-87654321" {
		t.Fatalf("env API key was not applied, got %q", cfg.Providers["openai"].APIKey)
	}
	outPath := filepath.Join(dir, "saved.json")
	if err := cfg.Save(outPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	saved, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(saved), "env-key-87654321") {
		t.Fatal("Save persisted the env-supplied API key to disk")
	}
	if strings.Contains(string(saved), "987654321:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB") {
		t.Fatal("Save persisted the env-supplied Telegram token to disk")
	}
	if !strings.Contains(string(saved), fileKey) {
		t.Fatal("Save lost the file-stored API key (should restore it)")
	}
	if !strings.Contains(string(saved), fileToken) {
		t.Fatal("Save lost the file-stored Telegram token (should restore it)")
	}
}

// TestSavePersistsExplicitlySetKey: a key changed after load (the
// `orion config set-api-key` path) no longer matches its env value and
// must be persisted normally.
func TestSavePersistsExplicitlySetKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfgJSON := `{
		"default_provider": "openai",
		"providers": {"openai": {"name": "openai", "model": "gpt-4o-mini", "api_key": "file-key-12345678"}},
		"data_dir": "` + dir + `",
		"max_steps": 20,
		"timeout_seconds": 120
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("ORION_API_KEY", "env-key-87654321")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Providers["openai"]
	p.APIKey = "explicit-key-99999999"
	cfg.Providers["openai"] = p
	outPath := filepath.Join(dir, "saved.json")
	if err := cfg.Save(outPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	saved, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(saved), "explicit-key-99999999") {
		t.Fatalf("explicitly set key was not persisted: %s", saved)
	}
}
