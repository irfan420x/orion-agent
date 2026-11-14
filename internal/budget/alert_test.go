// Daily budget threshold alert tests (F-A18-11, rc24): the
// threshold evaluation, the once-per-chat-per-day rule, the day
// rollover, and the persistence of the "alerted today" flag across
// a restart (a fresh Budget on the same file must not re-alert
// for a day that already alerted).
package budget

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
)

// TestCheckAlertBelowThreshold: usage under the threshold never
// fires, however many records land.
func TestCheckAlertBelowThreshold(t *testing.T) {
	b := New(0) // unlimited budget: the alert must not care
	b.SetAlertThreshold(100)
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 60}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if used, threshold, crossed := b.CheckAlert(7); crossed {
		t.Errorf("CheckAlert(7) = crossed with used=%d threshold=%d, want no alert below threshold", used, threshold)
	}
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 39}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if used, _, crossed := b.CheckAlert(7); crossed {
		t.Errorf("CheckAlert(7) = crossed at used=%d (threshold 100), want no alert at 99", used)
	}
	// Another chat with no usage never fires either.
	if _, _, crossed := b.CheckAlert(8); crossed {
		t.Error("CheckAlert(8) = crossed for a chat with no usage, want no alert")
	}
}

// TestCheckAlertCrossingOnce: the turn that reaches the threshold
// fires; after MarkAlerted the same day stays silent even as usage
// keeps climbing.
func TestCheckAlertCrossingOnce(t *testing.T) {
	b := New(100000) // finite budget: the alert is independent of it
	b.SetAlertThreshold(100)
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 60}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); crossed {
		t.Error("CheckAlert(7) = crossed at 60, want no alert below threshold")
	}
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 40}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	used, threshold, crossed := b.CheckAlert(7)
	if !crossed {
		t.Fatalf("CheckAlert(7) = no alert at used=%d threshold=%d, want crossing at exactly 100", used, threshold)
	}
	if used != 100 || threshold != 100 {
		t.Errorf("CheckAlert(7) = used=%d threshold=%d, want 100/100", used, threshold)
	}
	// CheckAlert is a pure check: before marking, it still
	// reports the crossing (the caller marks only after a
	// successful delivery).
	if _, _, crossed := b.CheckAlert(7); !crossed {
		t.Error("CheckAlert(7) before MarkAlerted = no alert, want the crossing still reported")
	}
	if err := b.MarkAlerted(7); err != nil {
		t.Fatalf("MarkAlerted: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); crossed {
		t.Error("CheckAlert(7) after MarkAlerted = crossed, want silence for the rest of the day")
	}
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 500}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); crossed {
		t.Error("CheckAlert(7) = crossed after more usage post-alert, want exactly one alert per day")
	}
	// Another chat is unaffected by this chat's alert.
	if err := b.RecordUsage(8, model.Usage{TotalTokens: 150}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b.CheckAlert(8); !crossed {
		t.Error("CheckAlert(8) = no alert at 150, want its own crossing (per-chat, not global)")
	}
}

// TestCheckAlertDisabled: threshold 0 (the default) or negative
// never fires, whatever the usage.
func TestCheckAlertDisabled(t *testing.T) {
	b := New(0)
	if got := b.AlertThreshold(); got != 0 {
		t.Errorf("AlertThreshold() = %d, want 0 (disabled by default)", got)
	}
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 1000000}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); crossed {
		t.Error("CheckAlert(7) = crossed with threshold 0, want disabled")
	}
	b.SetAlertThreshold(-5)
	if _, _, crossed := b.CheckAlert(7); crossed {
		t.Error("CheckAlert(7) = crossed with threshold -5, want disabled")
	}
	// MarkAlerted on a disabled threshold is harmless.
	if err := b.MarkAlerted(7); err != nil {
		t.Errorf("MarkAlerted with alert disabled = %v, want nil", err)
	}
}

// TestCheckAlertUnlimitedLimit: the live case — budget unlimited
// (limit 0) with a threshold set — the alert still fires. The
// limit plays no part in the decision.
func TestCheckAlertUnlimitedLimit(t *testing.T) {
	b := New(0)
	if b.Limit() != 0 {
		t.Fatalf("Limit() = %d, want 0 (unlimited)", b.Limit())
	}
	b.SetAlertThreshold(100)
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 100}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); !crossed {
		t.Error("CheckAlert(7) = no alert with unlimited budget at threshold, want the alert to fire")
	}
}

// TestCheckAlertFiniteLimit: a finite limit with a threshold
// below it fires on crossing; a threshold above the limit still
// fires when the final over-limit turn's spend reaches it (the
// alert is usage-based, the limit only refuses future turns).
func TestCheckAlertFiniteLimit(t *testing.T) {
	b := New(1000)
	b.SetAlertThreshold(100)
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 100}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); !crossed {
		t.Error("CheckAlert(7) = no alert with finite limit at threshold, want the alert to fire")
	}

	above := New(100)
	above.SetAlertThreshold(1000)
	// The record itself is over the limit, so it errors — but
	// the spend is still recorded (see RecordUsage), so the
	// alert can still fire on it.
	if err := above.RecordUsage(7, model.Usage{TotalTokens: 1000}, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("RecordUsage over the limit = %v, want BudgetExceededError (spend still recorded)", err)
	}
	if _, _, crossed := above.CheckAlert(7); !crossed {
		t.Error("CheckAlert(7) = no alert with threshold above a finite limit at 1000 used, want the alert to fire")
	}
}

// TestCheckAlertDayRollover: the alert fires again on the next UTC
// day, after the usage and the flag reset.
func TestCheckAlertDayRollover(t *testing.T) {
	day1 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	now := day1
	b := NewWithClock(0, func() time.Time { return now })
	b.SetAlertThreshold(100)
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 150}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); !crossed {
		t.Fatal("CheckAlert(7) day 1 = no alert, want crossing")
	}
	if err := b.MarkAlerted(7); err != nil {
		t.Fatalf("MarkAlerted: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); crossed {
		t.Fatal("CheckAlert(7) day 1 after mark = crossed, want silence")
	}
	now = day1.Add(24 * time.Hour) // next UTC day
	if used := b.Usage(7); used != 0 {
		t.Fatalf("Usage(7) next day = %d, want 0 (day reset)", used)
	}
	if _, _, crossed := b.CheckAlert(7); crossed {
		t.Error("CheckAlert(7) next day with no usage = crossed, want no alert before any spend")
	}
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 100}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b.CheckAlert(7); !crossed {
		t.Error("CheckAlert(7) next day at threshold = no alert, want the alert to fire again on the new day")
	}
}

// TestCheckAlertPersistsAcrossRestart: the "alerted today" flag
// survives a restart through budget.json — a fresh Budget on the
// same file does not re-alert for a day that already alerted,
// while a chat that crossed but was never marked still alerts.
func TestCheckAlertPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	b := New(0)
	b.SetAlertThreshold(100)
	if _, err := b.SetFile(path); err != nil {
		t.Fatalf("SetFile: %v", err)
	}
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 150}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if err := b.RecordUsage(8, model.Usage{TotalTokens: 150}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// Chat 7 is alerted (and marked); chat 8 crossed but its
	// alert was never delivered, so it stays unmarked.
	if _, _, crossed := b.CheckAlert(7); !crossed {
		t.Fatal("CheckAlert(7) = no alert before restart, want crossing")
	}
	if err := b.MarkAlerted(7); err != nil {
		t.Fatalf("MarkAlerted: %v", err)
	}

	// Restart: a fresh Budget on the same file.
	b2 := New(0)
	b2.SetAlertThreshold(100)
	if n, err := b2.SetFile(path); err != nil {
		t.Fatalf("SetFile on restart: %v", err)
	} else if n != 2 {
		t.Errorf("SetFile restored %d chat(s), want 2", n)
	}
	if used := b2.Usage(7); used != 150 {
		t.Errorf("Usage(7) after restart = %d, want 150", used)
	}
	if _, _, crossed := b2.CheckAlert(7); crossed {
		t.Error("CheckAlert(7) after restart = crossed, want no re-alert for a day that already alerted")
	}
	if _, _, crossed := b2.CheckAlert(8); !crossed {
		t.Error("CheckAlert(8) after restart = no alert, want the crossing still reported (never marked)")
	}
}

// TestCheckAlertStaleFileLoadsUnalerted: a budget file from a
// previous UTC day loads as empty — yesterday's alert never
// suppresses today's.
func TestCheckAlertStaleFileLoadsUnalerted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	day1 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	now := day1
	b := NewWithClock(0, func() time.Time { return now })
	b.SetAlertThreshold(100)
	if _, err := b.SetFile(path); err != nil {
		t.Fatalf("SetFile: %v", err)
	}
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 150}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if err := b.MarkAlerted(7); err != nil {
		t.Fatalf("MarkAlerted: %v", err)
	}

	now = day1.Add(24 * time.Hour)
	b2 := NewWithClock(0, func() time.Time { return now })
	b2.SetAlertThreshold(100)
	if n, err := b2.SetFile(path); err != nil {
		t.Fatalf("SetFile next day: %v", err)
	} else if n != 0 {
		t.Errorf("SetFile next day restored %d chat(s), want 0 (stale day loads empty)", n)
	}
	if err := b2.RecordUsage(7, model.Usage{TotalTokens: 100}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, crossed := b2.CheckAlert(7); !crossed {
		t.Error("CheckAlert(7) next day = no alert, want the alert to fire (yesterday's flag must not carry over)")
	}
}
