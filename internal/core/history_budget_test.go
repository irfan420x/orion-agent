// History-budget derivation tests (provider-auto context
// window): an explicit MaxHistoryTokens overrides; otherwise the
// budget is half the active provider's declared window, clamped
// to [1024, 1000000]; a provider that declares no window falls
// back to the legacy 32768.
package core

import (
	"fmt"
	"testing"

	"github.com/irfan/orion/internal/tools"
)

// windowProvider is a fakeProvider that also declares a context
// window, so derivation can be tested without a real provider.
type windowProvider struct {
	*fakeProvider
	window int
}

func (p *windowProvider) ContextWindow() int { return p.window }

func TestHistoryBudgetDerivedFromProviderWindow(t *testing.T) {
	cases := []struct {
		window int
		want   int
	}{
		{65536, 32768},    // e.g. a 64K-window provider
		{131072, 65536},   // Ollama llama3.1
		{200000, 100000},  // Anthropic 4.5 models
		{1000000, 500000}, // DeepSeek (1M window): half is 500K
		{128000, 64000},   // OpenAI gpt-4o-mini
		{1047576, 523788}, // OpenAI gpt-4.1-mini
		{32768, 16384},    // Ollama qwen2.5-0.5b
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("window-%d", tc.window), func(t *testing.T) {
			cfg := testConfig(5)
			cfg.MaxHistoryTokens = 0 // auto
			a := newTestAgent(t, cfg, &windowProvider{fakeProvider: &fakeProvider{}, window: tc.window}, tools.NewRegistry(), newFakeStore())
			if got := a.maxHistoryTokens(); got != tc.want {
				t.Fatalf("window %d: history budget = %d, want %d", tc.window, got, tc.want)
			}
			if got := a.HistoryBudget(); got != tc.want {
				t.Fatalf("window %d: HistoryBudget() = %d, want %d", tc.window, got, tc.want)
			}
		})
	}
}

func TestHistoryBudgetOverrideBeatsDerivation(t *testing.T) {
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 8192
	a := newTestAgent(t, cfg, &windowProvider{fakeProvider: &fakeProvider{}, window: 131072}, tools.NewRegistry(), newFakeStore())
	if got := a.HistoryBudget(); got != 8192 {
		t.Fatalf("explicit 8192 with a 131072-token window: HistoryBudget() = %d, want 8192", got)
	}
}

func TestHistoryBudgetFallbackForNonDeclaringProvider(t *testing.T) {
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 0 // auto, but the fake declares nothing
	a := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	if got := a.HistoryBudget(); got != 32768 {
		t.Fatalf("non-declaring provider, auto budget: HistoryBudget() = %d, want 32768 (legacy fallback)", got)
	}
}

func TestHistoryBudgetClamping(t *testing.T) {
	cases := []struct {
		window int
		want   int
	}{
		{100, 1024},        // half is 50: clamped up to the minimum
		{2048, 1024},       // half is exactly the minimum
		{3000000, 1000000}, // half is 1.5M: clamped down to the maximum
		{2000000, 1000000}, // half is exactly the maximum
	}
	for _, tc := range cases {
		cfg := testConfig(5)
		cfg.MaxHistoryTokens = 0
		a := newTestAgent(t, cfg, &windowProvider{fakeProvider: &fakeProvider{}, window: tc.window}, tools.NewRegistry(), newFakeStore())
		if got := a.HistoryBudget(); got != tc.want {
			t.Fatalf("window %d: HistoryBudget() = %d, want %d (clamped)", tc.window, got, tc.want)
		}
	}
}

func TestHistoryBudgetZeroWindowFallsBack(t *testing.T) {
	// A provider that implements the interface but reports a
	// non-positive window must not produce a 0/negative budget:
	// it falls back exactly like a non-declaring provider.
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 0
	a := newTestAgent(t, cfg, &windowProvider{fakeProvider: &fakeProvider{}, window: 0}, tools.NewRegistry(), newFakeStore())
	if got := a.HistoryBudget(); got != 32768 {
		t.Fatalf("window 0: HistoryBudget() = %d, want 32768 (fallback)", got)
	}
}
