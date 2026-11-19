// History-budget ceiling tests (layer-0 cost reduction, P1):
// the provider-derived budget never exceeds
// historyBudgetCeilingTokens (32768), whatever the declared
// window — while an explicit MaxHistoryTokens (the deployer's
// deliberate choice) is never capped by the ceiling, and
// below-ceiling derivation is unchanged.
package core

import (
	"testing"

	"github.com/irfan/orion/internal/tools"
)

// TestHistoryBudgetCeilingCapsDerivedBudget: a derived budget is
// capped at the ceiling for every window whose half exceeds it —
// the live cases (OpenRouter 128K -> 64K, DeepSeek 1M -> 500K)
// and absurd windows alike. On the base commit the 1M-window
// case returns 500000, not the ceiling.
func TestHistoryBudgetCeilingCapsDerivedBudget(t *testing.T) {
	for _, window := range []int{128000, 131072, 200000, 1000000, 1047576, 2000000, 3000000} {
		cfg := testConfig(5)
		cfg.MaxHistoryTokens = 0 // auto
		a := newTestAgent(t, cfg, &windowProvider{fakeProvider: &fakeProvider{}, window: window}, tools.NewRegistry(), newFakeStore())
		if got := a.HistoryBudget(); got != historyBudgetCeilingTokens {
			t.Errorf("window %d: HistoryBudget() = %d, want ceiling %d", window, got, historyBudgetCeilingTokens)
		}
	}
}

// TestHistoryBudgetCeilingDoesNotCapExplicitOverride: an explicit
// MaxHistoryTokens above the ceiling passes through unchanged —
// the ceiling binds the derivation, never the deployer's choice.
// On the base commit this already passes (no ceiling existed);
// it pins the exemption so a future change cannot silently cap
// explicit values.
func TestHistoryBudgetCeilingDoesNotCapExplicitOverride(t *testing.T) {
	for _, explicit := range []int{32769, 100000, 500000} {
		cfg := testConfig(5)
		cfg.MaxHistoryTokens = explicit
		a := newTestAgent(t, cfg, &windowProvider{fakeProvider: &fakeProvider{}, window: 1000000}, tools.NewRegistry(), newFakeStore())
		if got := a.HistoryBudget(); got != explicit {
			t.Errorf("explicit %d with a 1M window: HistoryBudget() = %d, want %d (explicit values are never capped)", explicit, got, explicit)
		}
	}
}

// TestHistoryBudgetCeilingLeavesSmallWindowsAlone: below the
// ceiling the derivation is exactly half the window — the ceiling
// changes nothing for small-window providers.
func TestHistoryBudgetCeilingLeavesSmallWindowsAlone(t *testing.T) {
	cases := []struct {
		window int
		want   int
	}{
		{32768, 16384},
		{65536, 32768}, // half is exactly the ceiling
		{65534, 32767}, // half is one below the ceiling
		{10000, 5000},
	}
	for _, tc := range cases {
		cfg := testConfig(5)
		cfg.MaxHistoryTokens = 0 // auto
		a := newTestAgent(t, cfg, &windowProvider{fakeProvider: &fakeProvider{}, window: tc.window}, tools.NewRegistry(), newFakeStore())
		if got := a.HistoryBudget(); got != tc.want {
			t.Errorf("window %d: HistoryBudget() = %d, want %d", tc.window, got, tc.want)
		}
	}
}

// TestHistoryBudgetCeilingEqualsLegacyFallback: the ceiling is
// the legacy fail-closed budget (32768), so a derived budget on
// the largest window is exactly the budget a non-declaring
// provider gets — the ceiling restores the old default as the
// maximum, not just a number.
func TestHistoryBudgetCeilingEqualsLegacyFallback(t *testing.T) {
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 0
	declaring := newTestAgent(t, cfg, &windowProvider{fakeProvider: &fakeProvider{}, window: 1000000}, tools.NewRegistry(), newFakeStore())
	nonDeclaring := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	if got, want := declaring.HistoryBudget(), nonDeclaring.HistoryBudget(); got != want {
		t.Errorf("1M-window derived budget = %d, non-declaring fallback = %d, want equal (ceiling == legacy fallback)", got, want)
	}
}
