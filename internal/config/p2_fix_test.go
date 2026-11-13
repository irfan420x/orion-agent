// P2-O5 / P2-O9 tests (rc8): the allow-all time-box and the
// per-chat daily token budget config.
package config

import (
	"strings"
	"testing"
	"time"
)

// TestP2AllowAllDefaults: fresh defaults carry a 10-minute allow-all
// window and a 100K-token per-chat daily budget.
func TestP2AllowAllDefaults(t *testing.T) {
	cfg := Default()
	if cfg.AllowAllTelegramChatsWindowMinutes != DefaultAllowAllTelegramWindowMinutes {
		t.Errorf("AllowAllTelegramChatsWindowMinutes = %d, want %d (default)", cfg.AllowAllTelegramChatsWindowMinutes, DefaultAllowAllTelegramWindowMinutes)
	}
	if cfg.AllowAllWindow() != 10*time.Minute {
		t.Errorf("AllowAllWindow() = %s, want 10m", cfg.AllowAllWindow())
	}
	if cfg.PerChatDailyTokenBudget != DefaultPerChatDailyTokenBudget {
		t.Errorf("PerChatDailyTokenBudget = %d, want %d (default)", cfg.PerChatDailyTokenBudget, DefaultPerChatDailyTokenBudget)
	}
}

// TestP2AllowAllWindow: an explicit window is honoured; 0 means the
// 10-minute default (so hand-built configs validate).
func TestP2AllowAllWindow(t *testing.T) {
	cfg := Default()
	cfg.AllowAllTelegramChatsWindowMinutes = 30
	if cfg.AllowAllWindow() != 30*time.Minute {
		t.Errorf("AllowAllWindow() = %s, want 30m", cfg.AllowAllWindow())
	}
	cfg.AllowAllTelegramChatsWindowMinutes = 0
	if cfg.AllowAllWindow() != 10*time.Minute {
		t.Errorf("AllowAllWindow() with 0 = %s, want 10m (default)", cfg.AllowAllWindow())
	}
}

// TestP2AllowAllWindowValidation: out-of-range windows are rejected.
func TestP2AllowAllWindowValidation(t *testing.T) {
	for _, mins := range []int{-1, 121, 10000} {
		cfg := Default()
		cfg.AllowAllTelegramChatsWindowMinutes = mins
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate with window %d minutes = nil, want error", mins)
		}
	}
}

// TestP2AllowAllWarning: the warning is empty when the opt-in is off
// and LOUD when it is on — naming the risk, the auto-disable window
// and the fix.
func TestP2AllowAllWarning(t *testing.T) {
	cfg := Default()
	if w := cfg.AllowAllTelegramWarning(); w != "" {
		t.Errorf("AllowAllTelegramWarning with opt-in off = %q, want empty", w)
	}
	cfg.AllowAllTelegramChats = true
	w := cfg.AllowAllTelegramWarning()
	for _, want := range []string{
		"WARNING",
		"allow_all_telegram_chats",
		"telegram_allowed_chats",
		"10m0s",
		"shell_exec",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("allow-all warning %q does not mention %q", w, want)
		}
	}
	cfg.AllowAllTelegramChatsWindowMinutes = 45
	if w := cfg.AllowAllTelegramWarning(); !strings.Contains(w, "45m0s") {
		t.Errorf("allow-all warning with 45m window = %q, want it to name 45m0s", w)
	}
}

// TestP2PerChatBudgetValidation: 0 means unlimited, 1000-10M is the
// settable range, anything else is rejected.
func TestP2PerChatBudgetValidation(t *testing.T) {
	cfg := Default()
	cfg.PerChatDailyTokenBudget = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with budget 0 (unlimited) = %v, want nil", err)
	}
	for _, good := range []int{1000, 50000, 10000000} {
		cfg.PerChatDailyTokenBudget = good
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate with budget %d = %v, want nil", good, err)
		}
	}
	for _, bad := range []int{-1, 999, 10000001} {
		cfg.PerChatDailyTokenBudget = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate with budget %d = nil, want error", bad)
		}
	}
}
