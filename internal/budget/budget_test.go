package budget

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCheckPassesUnderLimit(t *testing.T) {
	b := New(1000)
	if err := b.Check(42, 500); err != nil {
		t.Fatalf("Check(42, 500) with limit 1000 = %v, want nil", err)
	}
	if err := b.Check(42, 1000); err != nil {
		t.Fatalf("Check(42, 1000) with 0 used = %v, want nil (exactly at the limit is allowed)", err)
	}
}

func TestCheckFailsOverLimit(t *testing.T) {
	b := New(1000)
	if err := b.Record(42, 800); err != nil {
		t.Fatalf("Record(42, 800) = %v, want nil", err)
	}
	if err := b.Check(42, 201); err == nil {
		t.Fatal("Check(42, 201) at 800/1000 = nil, want BudgetExceededError")
	} else {
		var be *BudgetExceededError
		if !errors.As(err, &be) {
			t.Fatalf("Check error = %T, want *BudgetExceededError", err)
		}
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Errorf("Check error does not wrap ErrBudgetExceeded")
		}
		if be.ChatID != 42 || be.Used != 800 || be.Requested != 201 || be.Limit != 1000 {
			t.Errorf("BudgetExceededError = %+v, want {42 800 201 1000}", be)
		}
	}
	// Exactly reaching the limit still passes.
	if err := b.Check(42, 200); err != nil {
		t.Errorf("Check(42, 200) at 800/1000 = %v, want nil", err)
	}
}

func TestRecordAccountsOverBudgetTurns(t *testing.T) {
	b := New(1000)
	// A turn that pushes the chat over the limit is recorded (the
	// spend happened) and reported, so the next turn is refused.
	if err := b.Record(7, 1200); err == nil {
		t.Fatal("Record(7, 1200) over a 1000 limit = nil, want error")
	}
	if got := b.Usage(7); got != 1200 {
		t.Errorf("Usage after over-budget Record = %d, want 1200 (spend is accounted)", got)
	}
	if err := b.Check(7, 0); err == nil {
		t.Error("Check(7, 0) at 1200/1000 = nil, want error (already over)")
	}
	if got := b.Remaining(7); got != 0 {
		t.Errorf("Remaining at 1200/1000 = %d, want 0", got)
	}
}

func TestBudgetIsPerChat(t *testing.T) {
	b := New(1000)
	if err := b.Record(1, 1000); err != nil {
		t.Fatalf("Record(1, 1000) = %v", err)
	}
	// Another chat is unaffected by the first chat's spend.
	if err := b.Check(2, 1000); err != nil {
		t.Errorf("Check(2, 1000) while chat 1 is at its limit = %v, want nil (per-chat)", err)
	}
	if err := b.Check(1, 1); err == nil {
		t.Error("Check(1, 1) at 1000/1000 = nil, want error")
	}
}

func TestUnlimitedBudget(t *testing.T) {
	for _, limit := range []int{0, -5} {
		b := New(limit)
		if err := b.Check(1, 100000000); err != nil {
			t.Errorf("Check with limit %d = %v, want nil (unlimited)", limit, err)
		}
		if err := b.Record(1, 100000000); err != nil {
			t.Errorf("Record with limit %d = %v, want nil (unlimited)", limit, err)
		}
		if got := b.Remaining(1); got != -1 {
			t.Errorf("Remaining with limit %d = %d, want -1 (unlimited)", limit, got)
		}
	}
}

func TestDayRolloverResetsUsage(t *testing.T) {
	now := time.Date(2026, 11, 13, 23, 30, 0, 0, time.UTC)
	b := NewWithClock(1000, func() time.Time { return now })
	if err := b.Record(42, 900); err != nil {
		t.Fatalf("Record = %v", err)
	}
	// Same day: still counted.
	if err := b.Check(42, 200); err == nil {
		t.Error("Check same day at 900/1000 for +200 = nil, want error")
	}
	// Past UTC midnight: a fresh day, usage reset.
	now = time.Date(2026, 11, 14, 0, 5, 0, 0, time.UTC)
	if got := b.Usage(42); got != 0 {
		t.Errorf("Usage after UTC midnight = %d, want 0", got)
	}
	if err := b.Check(42, 1000); err != nil {
		t.Errorf("Check after rollover = %v, want nil", err)
	}
	if got := b.Remaining(42); got != 1000 {
		t.Errorf("Remaining after rollover = %d, want 1000", got)
	}
}

func TestResetAtIsNextUTCMidnight(t *testing.T) {
	now := time.Date(2026, 11, 13, 15, 4, 5, 0, time.UTC)
	b := NewWithClock(1000, func() time.Time { return now })
	want := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	if got := b.ResetAt(); !got.Equal(want) {
		t.Errorf("ResetAt() = %v, want %v", got, want)
	}
}

func TestNegativeTokensRejected(t *testing.T) {
	b := New(1000)
	if err := b.Check(1, -1); err == nil {
		t.Error("Check with negative tokens = nil, want error")
	}
	if err := b.Record(1, -1); err == nil {
		t.Error("Record with negative tokens = nil, want error")
	}
	if got := b.Usage(1); got != 0 {
		t.Errorf("Usage after rejected negative Record = %d, want 0", got)
	}
}

func TestSetLimit(t *testing.T) {
	b := New(1000)
	if err := b.Record(1, 600); err != nil {
		t.Fatalf("Record = %v", err)
	}
	b.SetLimit(500)
	if err := b.Check(1, 0); err == nil {
		t.Error("Check after lowering limit below usage = nil, want error")
	}
	if b.Limit() != 500 {
		t.Errorf("Limit() = %d, want 500", b.Limit())
	}
	b.SetLimit(0)
	if err := b.Check(1, 1000000); err != nil {
		t.Errorf("Check after SetLimit(0) = %v, want nil (unlimited)", err)
	}
}

func TestConcurrentCheckRecord(t *testing.T) {
	b := New(1000000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(chat int64) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = b.Check(chat, 10)
				_ = b.Record(chat, 10)
			}
		}(int64(i))
	}
	wg.Wait()
	for i := 0; i < 8; i++ {
		if got := b.Usage(int64(i)); got != 2000 {
			t.Errorf("Usage(chat %d) = %d, want 2000", i, got)
		}
	}
}
