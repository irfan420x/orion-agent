// Fallback-chain and alias config tests.
//
// The chain is opt-in config: `fallback_providers` lists the providers
// a failed turn may continue on, in order, AFTER the daemon default
// (the default is always the chain head — listing it again is a
// no-op, not an error). Aliases are short names for a provider or a
// provider/model route. Both are validated fail-closed at load: an
// unknown or unconfigured provider in either field must fail startup
// with a clear error, never silently drop a chain entry the operator
// is counting on.
package config

import (
	"strings"
	"testing"
)

func fallbackTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := Default()
	cfg.DataDir = t.TempDir()
	// The default provider is ollama (no key needed); give the
	// key-requiring providers fake keys so route resolution can
	// build them in tests that need a provider instance.
	for _, name := range []string{"openai", "deepseek", "anthropic"} {
		p := cfg.Providers[name]
		p.APIKey = "test-fake-key"
		cfg.Providers[name] = p
	}
	return cfg
}

func TestFallbackChainDefaultIsHead(t *testing.T) {
	cfg := fallbackTestConfig(t)
	cfg.DefaultProvider = "deepseek"
	cfg.FallbackProviders = []string{"openai", "ollama"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	chain := cfg.FallbackChain()
	if len(chain) != 3 || chain[0] != "deepseek" || chain[1] != "openai" || chain[2] != "ollama" {
		t.Errorf("FallbackChain() = %v, want [deepseek openai ollama]", chain)
	}
	// Listing the default again is a no-op (it is the head by
	// construction), not a duplicate error.
	cfg.FallbackProviders = []string{"deepseek", "openai"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with default re-listed: %v", err)
	}
	if chain := cfg.FallbackChain(); len(chain) != 2 || chain[0] != "deepseek" || chain[1] != "openai" {
		t.Errorf("FallbackChain() with default re-listed = %v, want [deepseek openai]", chain)
	}
	// No fallbacks: the chain is just the default.
	cfg.FallbackProviders = nil
	if chain := cfg.FallbackChain(); len(chain) != 1 || chain[0] != "deepseek" {
		t.Errorf("FallbackChain() with no fallbacks = %v, want [deepseek]", chain)
	}
}

func TestFallbackProvidersValidation(t *testing.T) {
	cases := []struct {
		name      string
		fallbacks []string
		wantSub   string
	}{
		{"unknown provider", []string{"claude"}, "unknown provider"},
		{"duplicate", []string{"openai", "openai"}, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fallbackTestConfig(t)
			cfg.FallbackProviders = tc.fallbacks
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate with fallbacks %v: expected an error, got nil", tc.fallbacks)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantSub) {
				t.Errorf("Validate error = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestFallbackProviderMustBeConfigured(t *testing.T) {
	cfg := fallbackTestConfig(t)
	// Drop anthropic from the map entirely: naming it as a
	// fallback must fail validation (a chain entry that cannot
	// be built is worse than no entry — the turn would fail
	// over into a build error).
	delete(cfg.Providers, "anthropic")
	cfg.FallbackProviders = []string{"anthropic"}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate with an unconfigured fallback provider: expected an error, got nil")
	} else if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("Validate error = %v, want it to name anthropic", err)
	}
}

func TestProviderAliases(t *testing.T) {
	cfg := fallbackTestConfig(t)
	cfg.ProviderAliases = map[string]string{
		"fast":   "deepseek/deepseek-flash",
		"pro":    "deepseek/deepseek-v4-pro",
		"local":  "ollama",
		"openai": "openai/gpt-4o", // shadows a real provider name
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with an alias shadowing a provider name: expected an error, got nil")
	}
	delete(cfg.ProviderAliases, "openai")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with good aliases: %v", err)
	}
	// Alias resolution: exact match, then provider/model, then
	// a bare provider name.
	for _, tc := range []struct {
		route, wantProvider, wantModel string
	}{
		{"fast", "deepseek", "deepseek-flash"},
		{"pro", "deepseek", "deepseek-v4-pro"},
		{"local", "ollama", "llama3.1"}, // bare provider: entry model
		{"openai/gpt-4o", "openai", "gpt-4o"},
		{"deepseek", "deepseek", "deepseek-flash"},
		{"FAST", "deepseek", "deepseek-flash"}, // case-insensitive
		{"  pro  ", "deepseek", "deepseek-v4-pro"},
	} {
		provider, modelName, err := cfg.ResolveRoute(tc.route)
		if err != nil {
			t.Errorf("ResolveRoute(%q): %v", tc.route, err)
			continue
		}
		if provider != tc.wantProvider || modelName != tc.wantModel {
			t.Errorf("ResolveRoute(%q) = %q/%q, want %q/%q", tc.route, provider, modelName, tc.wantProvider, tc.wantModel)
		}
	}
}

func TestProviderAliasValidation(t *testing.T) {
	cases := []struct {
		name    string
		aliases map[string]string
		wantSub string
	}{
		{"unknown provider target", map[string]string{"x": "claude"}, "unknown provider"},
		{"unconfigured target", map[string]string{"x": "anthropic/gpt"}, "anthropic"}, // anthropic deleted below
		{"empty model part", map[string]string{"x": "deepseek/"}, "model"},
		{"empty alias value", map[string]string{"x": ""}, "empty"},
		{"alias with slash", map[string]string{"a/b": "deepseek"}, "alias"},
		{"alias with space", map[string]string{"a b": "deepseek"}, "alias"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fallbackTestConfig(t)
			if tc.name == "unconfigured target" {
				delete(cfg.Providers, "anthropic")
			}
			cfg.ProviderAliases = tc.aliases
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate with aliases %v: expected an error, got nil", tc.aliases)
			} else if !strings.Contains(strings.ToLower(err.Error()), tc.wantSub) {
				t.Errorf("Validate error = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestResolveRouteErrors(t *testing.T) {
	cfg := fallbackTestConfig(t)
	for _, route := range []string{"", "claude", "deepseek/", "/gpt-4o", "openai//x"} {
		if _, _, err := cfg.ResolveRoute(route); err == nil {
			t.Errorf("ResolveRoute(%q): expected an error, got nil", route)
		}
	}
	// An alias that points at an unconfigured provider fails at
	// resolve time too (hand-built configs skip Validate).
	cfg.ProviderAliases = map[string]string{"ghost": "anthropic"}
	delete(cfg.Providers, "anthropic")
	if _, _, err := cfg.ResolveRoute("ghost"); err == nil {
		t.Error("ResolveRoute(ghost) with anthropic unconfigured: expected an error, got nil")
	}
}
