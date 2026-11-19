// Daily budget defaults tests (layer-0 cost reduction, P2):
// the default per-chat daily budget is 100,000 and the default
// alert threshold is 80,000 (80% of it) — warning-first by
// default. An explicit 0 still disables the alert; the budget
// default is unchanged.
package config

import "testing"

// TestDailyBudgetDefaults: the default budget is 100,000 and the
// default alert threshold is 80% of it. Both fail on the base
// commit: the threshold default was 0 (disabled).
func TestDailyBudgetDefaults(t *testing.T) {
	cfg := Default()
	if cfg.PerChatDailyTokenBudget != 100000 {
		t.Errorf("PerChatDailyTokenBudget = %d, want 100000 (default)", cfg.PerChatDailyTokenBudget)
	}
	if cfg.PerChatDailyTokenBudget != DefaultPerChatDailyTokenBudget {
		t.Errorf("PerChatDailyTokenBudget = %d, want DefaultPerChatDailyTokenBudget %d", cfg.PerChatDailyTokenBudget, DefaultPerChatDailyTokenBudget)
	}
	if cfg.DailyBudgetAlertThresholdTokens != 80000 {
		t.Errorf("DailyBudgetAlertThresholdTokens = %d, want 80000 (80%% of the default budget)", cfg.DailyBudgetAlertThresholdTokens)
	}
	if cfg.DailyBudgetAlertThresholdTokens != 80000 {
		t.Errorf("DailyBudgetAlertThresholdTokens = %d, want DefaultDailyBudgetAlertThresholdTokens (80000)", cfg.DailyBudgetAlertThresholdTokens)
	}
	// The threshold must sit strictly below the budget it warns
	// about: a warning that can only fire at or after the refusal
	// is no warning.
	if cfg.DailyBudgetAlertThresholdTokens >= cfg.PerChatDailyTokenBudget {
		t.Errorf("default threshold %d is not below default budget %d", cfg.DailyBudgetAlertThresholdTokens, cfg.PerChatDailyTokenBudget)
	}
}

// TestDailyBudgetAlertThresholdZeroStillDisables: the layer-0
// change is a new DEFAULT, not a semantic change — an explicit 0
// still disables the alert, exactly as before.
func TestDailyBudgetAlertThresholdZeroStillDisables(t *testing.T) {
	cfg := Default()
	cfg.DailyBudgetAlertThresholdTokens = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with threshold 0 (disabled) = %v, want nil", err)
	}
	if cfg.DailyBudgetAlertThresholdTokens != 0 {
		t.Errorf("explicit threshold 0 = %d, want 0 (disabled)", cfg.DailyBudgetAlertThresholdTokens)
	}
}
