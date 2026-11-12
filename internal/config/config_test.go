package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	if cfg.DefaultProvider != "ollama" {
		t.Fatalf("default provider = %q, want ollama", cfg.DefaultProvider)
	}
	if _, ok := cfg.Providers["openai"]; !ok {
		t.Fatal("default config should include openai")
	}
	if _, ok := cfg.Providers["anthropic"]; !ok {
		t.Fatal("default config should include anthropic")
	}
}

func TestValidateErrors(t *testing.T) {
	cfg := Default()
	cfg.MaxSteps = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("MaxSteps=0 should fail validation")
	}
	cfg = Default()
	cfg.DefaultProvider = "nosuch"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown default provider should fail validation")
	}
	cfg = Default()
	cfg.TimeoutSeconds = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("TimeoutSeconds=0 should fail validation")
	}
	cfg = Default()
	badTemp := 2.5
	cfg.Temperature = &badTemp
	if err := cfg.Validate(); err == nil {
		t.Fatal("Temperature=2.5 should fail validation")
	}
	cfg = Default()
	zeroTemp := 0.0
	cfg.Temperature = &zeroTemp
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Temperature=0 should validate, got %v", err)
	}
}

func TestLoadMissingReturnsDefault(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should return default, got %v", err)
	}
	if cfg.DefaultProvider != "ollama" {
		t.Fatalf("got provider %q", cfg.DefaultProvider)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.DefaultProvider = "openai"
	cfg.Model = "gpt-4o"
	// openai requires an API key; use an obviously fake placeholder.
	p := cfg.Providers["openai"]
	p.APIKey = "test-key-placeholder"
	cfg.Providers["openai"] = p
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config file should be 0600, got %v %v", st.Mode(), err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.DefaultProvider != "openai" || loaded.Model != "gpt-4o" {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
}

func TestLoadCorruptErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("corrupt config should error")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("ORION_PROVIDER", "anthropic")
	t.Setenv("ORION_MODEL", "claude-test")
	t.Setenv("ORION_API_KEY", "test-key-env")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultProvider != "anthropic" {
		t.Fatalf("ORION_PROVIDER override failed: %q", cfg.DefaultProvider)
	}
	if cfg.Model != "claude-test" {
		t.Fatalf("ORION_MODEL override failed: %q", cfg.Model)
	}
	p, err := cfg.ActiveProvider()
	if err != nil {
		t.Fatalf("ActiveProvider: %v", err)
	}
	if p.APIKey != "test-key-env" {
		t.Fatal("ORION_API_KEY override failed")
	}
}

func TestActiveProviderModelOverride(t *testing.T) {
	cfg := Default()
	cfg.DefaultProvider = "openai"
	cfg.Model = "custom-model"
	p, err := cfg.ActiveProvider()
	if err != nil {
		t.Fatalf("ActiveProvider: %v", err)
	}
	// The provider's own model wins over the global one (same rule as
	// Validate); the global model is only the fallback.
	if p.Model != "gpt-4o-mini" {
		t.Fatalf("provider model should win, got %q", p.Model)
	}
	// With no per-provider model, the global model is the fallback.
	cfg.Providers["openai"] = ProviderConfig{Name: "openai", APIKey: "test-key"}
	p, err = cfg.ActiveProvider()
	if err != nil {
		t.Fatalf("ActiveProvider: %v", err)
	}
	if p.Model != "custom-model" {
		t.Fatalf("global model should be the fallback, got %q", p.Model)
	}
}

// fakeTelegramToken is a syntactically valid but fake Telegram bot
// token: numeric id, colon, 35 characters. Never a real credential.
const fakeTelegramToken = "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestValidateUnknownProviders(t *testing.T) {
	// Unknown default provider.
	cfg := Default()
	cfg.DefaultProvider = "gpt-x"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown default provider: err=%v, want 'unknown provider'", err)
	}

	// Unknown provider lurking in the providers map, even when it is
	// not the default, must not slip through.
	cfg = Default()
	cfg.Providers["gpt-x"] = ProviderConfig{Name: "gpt-x", Model: "whatever"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown provider in map: err=%v, want 'unknown provider'", err)
	}

	// Provider entry whose Name disagrees with its map key.
	cfg = Default()
	cfg.Providers["openai"] = ProviderConfig{Name: "anthropic", Model: "gpt-4o-mini", APIKey: "test-key-placeholder"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mismatched name") {
		t.Fatalf("name mismatch: err=%v, want 'mismatched name'", err)
	}
}

func TestValidateEmptyModel(t *testing.T) {
	// Default provider with no model anywhere: per-provider model
	// empty and no global model.
	cfg := Default()
	p := cfg.Providers["ollama"]
	p.Model = ""
	cfg.Providers["ollama"] = p
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("empty model: err=%v, want 'no model'", err)
	}

	// A global model covers a provider entry with no model of its own.
	cfg.Model = "llama3.1"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("global model should satisfy validation: %v", err)
	}

	// ActiveProvider must also refuse an empty effective model.
	cfg = Default()
	p = cfg.Providers["ollama"]
	p.Model = ""
	cfg.Providers["ollama"] = p
	if _, err := cfg.ActiveProvider(); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("ActiveProvider empty model: err=%v, want 'no model'", err)
	}
}

func TestValidateAPIKeyRequired(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		cfg := Default()
		cfg.DefaultProvider = provider
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "API key") {
			t.Fatalf("%s without API key: err=%v, want 'API key'", provider, err)
		}
		// Whitespace-only key is no key at all.
		p := cfg.Providers[provider]
		p.APIKey = "   "
		cfg.Providers[provider] = p
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "API key") {
			t.Fatalf("%s with blank API key: err=%v, want 'API key'", provider, err)
		}
		// A (fake) key satisfies validation.
		p.APIKey = "test-key-placeholder"
		cfg.Providers[provider] = p
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s with API key should validate: %v", provider, err)
		}
	}

	// Ollama is local and needs no key.
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ollama without API key should validate: %v", err)
	}
}

func TestValidateTelegramTokenFormat(t *testing.T) {
	bad := []string{
		"not-a-token",
		"123:short",
		"abc:" + strings.Repeat("A", 35),             // non-numeric bot id
		"123456789:" + strings.Repeat("A", 19),       // secret below the 20-char floor
		"123456789:" + strings.Repeat("A", 65),       // secret above the 64-char ceiling
		"123456789:" + strings.Repeat("A", 34) + "!", // illegal character
		"123456789 " + strings.Repeat("A", 35),       // missing colon
	}
	for _, tok := range bad {
		cfg := Default()
		cfg.TelegramToken = tok
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "telegram_token") {
			t.Fatalf("telegram token %q: err=%v, want 'telegram_token'", tok, err)
		}
	}

	// Well-formed fake tokens pass across the accepted 20-64 char
	// secret range, with or without surrounding spaces.
	valid := []string{
		fakeTelegramToken, // 35 chars (classic shape)
		"  " + fakeTelegramToken + "  ",
		"123456789:" + strings.Repeat("A", 20), // floor of the range
		"123456789:" + strings.Repeat("A", 34), // one short of classic
		"123456789:" + strings.Repeat("A", 36), // one long of classic
		"123456789:" + strings.Repeat("A", 43), // plausible future shape
		"123456789:" + strings.Repeat("A", 64), // ceiling of the range
	}
	for _, tok := range valid {
		cfg := Default()
		cfg.TelegramToken = tok
		if err := cfg.Validate(); err != nil {
			t.Fatalf("telegram token %q should validate: %v", tok, err)
		}
	}

	// No token at all is fine: Telegram is optional.
	if err := Default().Validate(); err != nil {
		t.Fatalf("empty telegram token should validate: %v", err)
	}
}

func TestValidateDataDir(t *testing.T) {
	cfg := Default()
	cfg.DataDir = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "data_dir") {
		t.Fatalf("empty data_dir: err=%v, want 'data_dir'", err)
	}

	cfg = Default()
	cfg.DataDir = "relative/path"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative data_dir: err=%v, want 'absolute'", err)
	}

	// A data dir that exists as a regular file is unusable.
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = Default()
	cfg.DataDir = file
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file as data_dir: err=%v, want 'not a directory'", err)
	}

	// A non-existent absolute dir is fine: it is created on use.
	cfg = Default()
	cfg.DataDir = filepath.Join(dir, "does", "not", "exist", "yet")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("missing data_dir should validate: %v", err)
	}
}

func TestValidateBaseURL(t *testing.T) {
	bad := []string{
		"ftp://example.com",
		"localhost:11434", // no scheme
		"http://",         // no host
		"://bad",
	}
	for _, u := range bad {
		cfg := Default()
		p := cfg.Providers["ollama"]
		p.BaseURL = u
		cfg.Providers["ollama"] = p
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "base_url") {
			t.Fatalf("base_url %q: err=%v, want 'base_url'", u, err)
		}
	}
	cfg := Default()
	p := cfg.Providers["ollama"]
	p.BaseURL = "http://localhost:11434"
	cfg.Providers["ollama"] = p
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid base_url should validate: %v", err)
	}
}

func TestLoadRejectsBadCombos(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"unknown provider": `{"default_provider": "gpt-x", "providers": {"gpt-x": {"name": "gpt-x", "model": "m"}}, "data_dir": "/tmp/x", "max_steps": 20, "timeout_seconds": 120}`,
		"openai no key":    `{"default_provider": "openai", "providers": {"openai": {"name": "openai", "model": "gpt-4o-mini"}}, "data_dir": "/tmp/x", "max_steps": 20, "timeout_seconds": 120}`,
		"bad base_url":     `{"default_provider": "ollama", "providers": {"ollama": {"name": "ollama", "model": "llama3.1", "base_url": "ftp://example.com"}}, "data_dir": "/tmp/x", "max_steps": 20, "timeout_seconds": 120}`,
		"bad token":        `{"default_provider": "ollama", "providers": {"ollama": {"name": "ollama", "model": "llama3.1"}}, "telegram_token": "nope", "data_dir": "/tmp/x", "max_steps": 20, "timeout_seconds": 120}`,
		"relative data":    `{"default_provider": "ollama", "providers": {"ollama": {"name": "ollama", "model": "llama3.1"}}, "data_dir": "rel/path", "max_steps": 20, "timeout_seconds": 120}`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("Load(%s): got nil error, want validation error", name)
			}
		})
	}
}

func TestActiveProviderFillsEmptyName(t *testing.T) {
	cfg := Default()
	p := cfg.Providers["ollama"]
	p.Name = ""
	cfg.Providers["ollama"] = p
	active, err := cfg.ActiveProvider()
	if err != nil {
		t.Fatalf("ActiveProvider: %v", err)
	}
	if active.Name != "ollama" {
		t.Fatalf("ActiveProvider name = %q, want %q", active.Name, "ollama")
	}
}

// writeConfigFile writes contents as a config file in dir and returns
// its path. Env overrides are cleared so the file alone decides.
func writeConfigFile(t *testing.T, dir, contents string) string {
	t.Helper()
	t.Setenv("ORION_PROVIDER", "")
	t.Setenv("ORION_API_KEY", "")
	t.Setenv("ORION_MODEL", "")
	t.Setenv("ORION_TELEGRAM_TOKEN", "")
	t.Setenv("ORION_DATA_DIR", "")
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadMergesPartialProviderEntries (P1-8): a file that sets only
// some fields of a provider entry must keep the defaults for the rest,
// not wipe them.
func TestLoadMergesPartialProviderEntries(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")

	// Partial openai entry (only an API key): default model survives
	// (openai's default base URL is empty, so it stays empty).
	path := writeConfigFile(t, dir, `{
		"default_provider": "openai",
		"providers": {"openai": {"api_key": "test-key-placeholder"}},
		"data_dir": "`+dataDir+`"
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load partial openai entry: %v", err)
	}
	p := cfg.Providers["openai"]
	if p.Model != "gpt-4o-mini" {
		t.Errorf("partial openai entry: model = %q, want default %q", p.Model, "gpt-4o-mini")
	}
	if p.BaseURL != "" {
		t.Errorf("partial openai entry: base_url = %q, want default %q", p.BaseURL, "")
	}
	if p.APIKey != "test-key-placeholder" {
		t.Errorf("partial openai entry: api_key was not kept from the file")
	}

	// Partial entry overriding only the model keeps the default base URL.
	path = writeConfigFile(t, dir, `{
		"default_provider": "ollama",
		"providers": {"ollama": {"model": "custom-llama"}},
		"data_dir": "`+dataDir+`"
	}`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load partial ollama entry: %v", err)
	}
	p = cfg.Providers["ollama"]
	if p.Model != "custom-llama" {
		t.Errorf("partial ollama entry: model = %q, want %q", p.Model, "custom-llama")
	}
	if p.BaseURL != "http://localhost:11434" {
		t.Errorf("partial ollama entry: base_url = %q, want default %q", p.BaseURL, "http://localhost:11434")
	}

	// A full override still wins outright.
	path = writeConfigFile(t, dir, `{
		"default_provider": "ollama",
		"providers": {"ollama": {"name": "ollama", "model": "other-model", "base_url": "http://example.com:11434"}},
		"data_dir": "`+dataDir+`"
	}`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load full ollama override: %v", err)
	}
	p = cfg.Providers["ollama"]
	if p.Model != "other-model" || p.BaseURL != "http://example.com:11434" {
		t.Errorf("full override: got model=%q base_url=%q, want other-model / http://example.com:11434", p.Model, p.BaseURL)
	}

	// A file mentioning only one provider still ends up with every
	// known provider present with its defaults.
	path = writeConfigFile(t, dir, `{
		"default_provider": "anthropic",
		"providers": {"anthropic": {"api_key": "test-key-placeholder"}},
		"data_dir": "`+dataDir+`"
	}`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load single-provider file: %v", err)
	}
	for _, name := range []string{"openai", "anthropic", "ollama"} {
		if _, ok := cfg.Providers[name]; !ok {
			t.Errorf("single-provider file: provider %q missing after load", name)
		}
	}
	if cfg.Providers["anthropic"].Model != "claude-sonnet-4-5" {
		t.Errorf("single-provider file: anthropic model = %q, want default", cfg.Providers["anthropic"].Model)
	}

	// providers: null must not wipe the defaults either.
	path = writeConfigFile(t, dir, `{
		"default_provider": "ollama",
		"providers": null,
		"data_dir": "`+dataDir+`"
	}`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load providers:null: %v", err)
	}
	if cfg.Providers["ollama"].Model != "llama3.1" {
		t.Errorf("providers:null: ollama model = %q, want default", cfg.Providers["ollama"].Model)
	}
}

// TestDefaultHomeUnset (P2-35): with no home directory, the error must
// surface at the source instead of a relative ".orion" data dir.
func TestDefaultHomeUnset(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := DefaultE(); err == nil || !strings.Contains(err.Error(), "home") {
		t.Fatalf("DefaultE without HOME: err=%v, want an error mentioning home", err)
	}
	if cfg := Default(); cfg.DataDir != "" {
		t.Fatalf("Default() without HOME: DataDir=%q, want empty (so Validate rejects it) instead of a relative .orion path", cfg.DataDir)
	}
	if _, err := DefaultPath(); err == nil || !strings.Contains(err.Error(), "home") {
		t.Fatalf("DefaultPath without HOME: err=%v, want an error mentioning home", err)
	}
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "home") {
		t.Fatalf("Load(\"\") without HOME: err=%v, want an error mentioning home", err)
	}
}

// TestKnownProviderHelpers (P2-37): the provider set is consulted
// through helpers, case-insensitively, and the name list is a fresh
// copy that callers cannot use to mutate the set.
func TestKnownProviderHelpers(t *testing.T) {
	for _, name := range []string{"openai", "OpenAI", "OPENAI", " anthropic ", "ollama"} {
		if !KnownProvider(name) {
			t.Errorf("KnownProvider(%q) = false, want true", name)
		}
	}
	if KnownProvider("gpt-x") || KnownProvider("") {
		t.Error("KnownProvider must reject unknown/empty names")
	}
	names := KnownProviderNames()
	want := []string{"anthropic", "ollama", "openai"}
	if len(names) != len(want) {
		t.Fatalf("KnownProviderNames() = %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("KnownProviderNames() = %v, want %v (sorted)", names, want)
		}
	}
	// Mutating the returned slice must not change the next call.
	names[0] = "bogus"
	if got := KnownProviderNames(); got[0] != "anthropic" {
		t.Fatalf("mutating KnownProviderNames() result leaked: next call = %v", got)
	}
}
