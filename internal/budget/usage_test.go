package budget

import (
	"errors"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
)

// TestRecordUsageKeepsSplit verifies the rc9 split accounting: the
// budget meter is TotalTokens (subagent tokens already folded in by
// the agent — they must NOT be added a second time), while the
// prompt/completion/subagent split is kept separately for the USD
// estimate.
func TestRecordUsageKeepsSplit(t *testing.T) {
	b := New(1000)
	u := model.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	if err := b.RecordUsage(42, u, 42); err != nil {
		t.Fatalf("RecordUsage = %v, want nil", err)
	}
	// Total is 150, NOT 150+42: the agent already added the 42
	// subagent tokens to TotalTokens before reporting the turn.
	if got := b.Usage(42); got != 150 {
		t.Errorf("Usage = %d, want 150 (subagent tokens must not be double-counted)", got)
	}
	du := b.DayUsage(42)
	if du.Prompt != 100 || du.Completion != 50 || du.Subagent != 42 || du.Total != 150 {
		t.Errorf("DayUsage = %+v, want {Prompt:100 Completion:50 Subagent:42 Total:150}", du)
	}
	// A second turn accumulates the split.
	if err := b.RecordUsage(42, model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, 0); err != nil {
		t.Fatalf("second RecordUsage = %v, want nil", err)
	}
	du = b.DayUsage(42)
	if du.Prompt != 110 || du.Completion != 55 || du.Subagent != 42 || du.Total != 165 {
		t.Errorf("DayUsage after second turn = %+v, want {110 55 42 165}", du)
	}
	// No usage yet: all zero.
	if du := b.DayUsage(99); du != (DayUsage{}) {
		t.Errorf("DayUsage(99) = %+v, want zero", du)
	}
}

// TestRecordUsageOverBudget mirrors Record's contract: the spend is
// accounted even when it breaches the limit, and the error names
// the breach.
func TestRecordUsageOverBudget(t *testing.T) {
	b := New(100)
	if err := b.RecordUsage(7, model.Usage{PromptTokens: 80, CompletionTokens: 40, TotalTokens: 120}, 10); err == nil {
		t.Fatal("RecordUsage over limit = nil, want BudgetExceededError")
	} else {
		var be *BudgetExceededError
		if !errors.As(err, &be) || !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("RecordUsage error = %T (%v), want *BudgetExceededError wrapping ErrBudgetExceeded", err, err)
		}
		if be.Used != 0 || be.Requested != 120 || be.Limit != 100 {
			t.Errorf("BudgetExceededError = %+v, want {Used:0 Requested:120 Limit:100}", be)
		}
	}
	if got := b.Usage(7); got != 120 {
		t.Errorf("Usage after over-budget RecordUsage = %d, want 120 (spend is accounted)", got)
	}
	if du := b.DayUsage(7); du.Prompt != 80 || du.Completion != 40 || du.Subagent != 10 {
		t.Errorf("DayUsage after over-budget RecordUsage = %+v, want split kept {80 40 10}", du)
	}
}

// TestRecordUsageNegativeCounts verifies negative counts record
// nothing and are reported as caller errors.
func TestRecordUsageNegativeCounts(t *testing.T) {
	b := New(1000)
	for _, tc := range []struct {
		name string
		u    model.Usage
		sub  int
	}{
		{"negative prompt", model.Usage{PromptTokens: -1}, 0},
		{"negative completion", model.Usage{CompletionTokens: -1}, 0},
		{"negative total", model.Usage{TotalTokens: -1}, 0},
		{"negative subagent", model.Usage{}, -1},
	} {
		if err := b.RecordUsage(1, tc.u, tc.sub); err == nil {
			t.Errorf("RecordUsage %s = nil, want error", tc.name)
		}
	}
	if got := b.Usage(1); got != 0 {
		t.Errorf("Usage after negative RecordUsage calls = %d, want 0", got)
	}
}

// TestDayUsageResetsAtMidnight verifies the split resets with the
// day, exactly like the total.
func TestDayUsageResetsAtMidnight(t *testing.T) {
	now := time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b := NewWithClock(1000, clock)
	if err := b.RecordUsage(5, model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, 3); err != nil {
		t.Fatalf("RecordUsage = %v", err)
	}
	now = time.Date(2026, 11, 14, 0, 30, 0, 0, time.UTC)
	if du := b.DayUsage(5); du != (DayUsage{}) {
		t.Errorf("DayUsage after midnight = %+v, want zero (new day)", du)
	}
}

// TestRecordDelegatesToSplit verifies the old total-only path still
// accounts, and leaves the split at zero (no split was reported).
func TestRecordDelegatesToSplit(t *testing.T) {
	b := New(1000)
	if err := b.Record(3, 200); err != nil {
		t.Fatalf("Record = %v", err)
	}
	if du := b.DayUsage(3); du.Total != 200 || du.Prompt != 0 || du.Completion != 0 || du.Subagent != 0 {
		t.Errorf("DayUsage after Record = %+v, want {Total:200, split zero}", du)
	}
}
