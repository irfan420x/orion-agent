// Daily budget alert threshold config tests (F-A18-11, rc24):
// daily_budget_alert_threshold_tokens is 0 (disabled) by default,
// 1-10M when set, and survives a JSON round-trip.
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDailyBudgetAlertThresholdDefaults: a fresh default config
// has the alert disabled (0) — an upgrade never starts alerting
// until the operator opts in.
func TestDailyBudgetAlertThresholdDefaults(t *testing.T) {
	cfg := Default()
	if cfg.DailyBudgetAlertThresholdTokens != 0 {
		t.Errorf("DailyBudgetAlertThresholdTokens = %d, want 0 (disabled by default)", cfg.DailyBudgetAlertThresholdTokens)
	}
}

// TestDailyBudgetAlertThresholdValidation: 0 means disabled,
// 1-10M is the settable range, anything else is rejected. The
// range is deliberately wider at the bottom than the budget's
// (1000) — a threshold is a notification, not a cap, so a small
// value for testing is legitimate.
func TestDailyBudgetAlertThresholdValidation(t *testing.T) {
	cfg := Default()
	cfg.DailyBudgetAlertThresholdTokens = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with threshold 0 (disabled) = %v, want nil", err)
	}
	for _, good := range []int{1, 100, 50000, 10000000} {
		cfg.DailyBudgetAlertThresholdTokens = good
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate with threshold %d = %v, want nil", good, err)
		}
	}
	for _, bad := range []int{-1, 10000001} {
		cfg.DailyBudgetAlertThresholdTokens = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate with threshold %d = nil, want error", bad)
		}
	}
}

// TestDailyBudgetAlertThresholdUnlimitedBudget: the threshold is
// independent of the budget limit — an unlimited budget (0) with
// a threshold set is a valid configuration (the live case).
func TestDailyBudgetAlertThresholdUnlimitedBudget(t *testing.T) {
	cfg := Default()
	cfg.PerChatDailyTokenBudget = 0
	cfg.DailyBudgetAlertThresholdTokens = 50000
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with unlimited budget + threshold 50000 = %v, want nil", err)
	}
}

// TestDailyBudgetAlertThresholdJSONRoundTrip: the field survives
// a config save/load cycle under its JSON name.
func TestDailyBudgetAlertThresholdJSONRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.DailyBudgetAlertThresholdTokens = 50000
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.DailyBudgetAlertThresholdTokens != 50000 {
		t.Errorf("round-tripped DailyBudgetAlertThresholdTokens = %d, want 50000", back.DailyBudgetAlertThresholdTokens)
	}
	if !strings.Contains(string(data), `"daily_budget_alert_threshold_tokens":50000`) {
		t.Errorf("marshalled config missing \"daily_budget_alert_threshold_tokens\":50000: %s", data)
	}
}
