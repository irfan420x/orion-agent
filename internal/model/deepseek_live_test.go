//go:build live

package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveDeepSeek makes ONE real call to DeepSeek using the key and
// endpoint from ~/.orion/config.json (the production config path).
// Run: go test -tags live -run TestLiveDeepSeek -count=1 ./internal/model/
// The key is never printed; only the reply and provider name are.
func TestLiveDeepSeek(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".orion", "config.json"))
	if err != nil {
		t.Skipf("no orion config: %v", err)
	}
	var cfg struct {
		DefaultProvider string `json:"default_provider"`
		Providers       map[string]struct {
			Name    string `json:"name"`
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// Not configured for DeepSeek (or DeepSeek has no key): skip,
	// don't fail. A `go test -tags live` run on a machine whose
	// default provider is Ollama/OpenAI must stay green; the live
	// DeepSeek test only applies where DeepSeek is the configured
	// default with a key.
	if cfg.DefaultProvider != "deepseek" {
		t.Skipf("default_provider = %q, want deepseek (live DeepSeek test not applicable)", cfg.DefaultProvider)
	}
	ds, ok := cfg.Providers["deepseek"]
	if !ok || strings.TrimSpace(ds.APIKey) == "" {
		t.Skip("no deepseek api_key in config (live DeepSeek test not applicable)")
	}
	p, err := NewProvider(ProviderConfig{Name: "deepseek", APIKey: ds.APIKey, BaseURL: ds.BaseURL, Model: ds.Model})
	if err != nil {
		t.Fatalf("NewProvider(deepseek): %v", err)
	}
	if p.Name() != "deepseek" {
		t.Fatalf("Name() = %q, want deepseek", p.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := p.Chat(ctx, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "You are a calculator. Reply with just the number, nothing else."},
			{Role: "user", Content: "What is 17*23+5?"},
		},
		MaxTokens: 50,
	})
	if err != nil {
		t.Fatalf("deepseek Chat: %v", err)
	}
	t.Logf("deepseek reply: %q", resp.Message.Content)
	if !strings.Contains(resp.Message.Content, "396") {
		t.Fatalf("reply %q does not contain 396", resp.Message.Content)
	}
}
