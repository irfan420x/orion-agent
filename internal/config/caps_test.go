package config

// Caps tests: MaxToolResultBytes and MaxHistoryTokens defaults,
// validation bounds, and save/load round-trip.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapsDefaults(t *testing.T) {
	cfg := Default()
	if cfg.MaxToolResultBytes != DefaultMaxToolResultBytes {
		t.Fatalf("MaxToolResultBytes = %d, want default %d", cfg.MaxToolResultBytes, DefaultMaxToolResultBytes)
	}
	if cfg.MaxHistoryTokens != DefaultMaxHistoryTokens {
		t.Fatalf("MaxHistoryTokens = %d, want default %d", cfg.MaxHistoryTokens, DefaultMaxHistoryTokens)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate with caps: %v", err)
	}
}

func TestCapsValidation(t *testing.T) {
	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"tool result 0", func(c *Config) { c.MaxToolResultBytes = 0 }},
		{"tool result negative", func(c *Config) { c.MaxToolResultBytes = -1 }},
		{"tool result below minimum", func(c *Config) { c.MaxToolResultBytes = 1023 }},
		{"tool result above maximum", func(c *Config) { c.MaxToolResultBytes = 10*1024*1024 + 1 }},
		{"history 0", func(c *Config) { c.MaxHistoryTokens = 0 }},
		{"history negative", func(c *Config) { c.MaxHistoryTokens = -5 }},
		{"history below minimum", func(c *Config) { c.MaxHistoryTokens = 1023 }},
		{"history above maximum", func(c *Config) { c.MaxHistoryTokens = 1000001 }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mut(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s should fail validation", tc.name)
			}
		})
	}
	good := []struct {
		name string
		mut  func(*Config)
	}{
		{"tool result minimum", func(c *Config) { c.MaxToolResultBytes = 1024 }},
		{"tool result maximum", func(c *Config) { c.MaxToolResultBytes = 10 * 1024 * 1024 }},
		{"history minimum", func(c *Config) { c.MaxHistoryTokens = 1024 }},
		{"history maximum", func(c *Config) { c.MaxHistoryTokens = 1000000 }},
		{"both small", func(c *Config) { c.MaxToolResultBytes = 4096; c.MaxHistoryTokens = 2048 }},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mut(cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("%s should validate, got %v", tc.name, err)
			}
		})
	}
}

func TestCapsSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.MaxToolResultBytes = 65536
	cfg.MaxHistoryTokens = 8192
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.MaxToolResultBytes != 65536 {
		t.Fatalf("MaxToolResultBytes = %d, want 65536", loaded.MaxToolResultBytes)
	}
	if loaded.MaxHistoryTokens != 8192 {
		t.Fatalf("MaxHistoryTokens = %d, want 8192", loaded.MaxHistoryTokens)
	}
}

func TestCapsLoadMissingFieldsUseDefaults(t *testing.T) {
	// A config file written before the caps existed (or one that
	// simply omits them) must load with the defaults, not zeroes:
	// Load unmarshals over Default(), so absent fields keep their
	// default values.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"default_provider":"ollama","data_dir":"` + dir + `","max_steps":20,"timeout_seconds":120}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxToolResultBytes != DefaultMaxToolResultBytes {
		t.Fatalf("MaxToolResultBytes = %d, want default %d", cfg.MaxToolResultBytes, DefaultMaxToolResultBytes)
	}
	if cfg.MaxHistoryTokens != DefaultMaxHistoryTokens {
		t.Fatalf("MaxHistoryTokens = %d, want default %d", cfg.MaxHistoryTokens, DefaultMaxHistoryTokens)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("loaded config should validate: %v", err)
	}
}

func TestCapsValidationErrorMentionsField(t *testing.T) {
	cfg := Default()
	cfg.MaxToolResultBytes = 10
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_tool_result_bytes") {
		t.Fatalf("expected error naming max_tool_result_bytes, got %v", err)
	}
	cfg = Default()
	cfg.MaxHistoryTokens = 10
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_history_tokens") {
		t.Fatalf("expected error naming max_history_tokens, got %v", err)
	}
}
