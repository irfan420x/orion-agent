// W3b (rc10) regression tests for Audit 5 F-A5-P2-19: the budget
// must survive a daemon restart — usage is persisted atomically and
// reloaded on start, and a stale day loads as empty.
package budget

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
)

// TestW3BBudgetPersistsAcrossRestart proves the persistence round
// trip: records auto-save to the file, and a fresh Budget on the
// same file restores today's totals — including the per-model split
// (F-A5-P2-18) — so a restart no longer resets every chat to zero.
func TestW3BBudgetPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	day := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return day }

	b1 := NewWithClock(100000, clock)
	if n, err := b1.SetFile(path); err != nil || n != 0 {
		t.Fatalf("SetFile (fresh) = %d/%v, want 0/nil", n, err)
	}
	if err := b1.RecordUsageForModel(7, "deepseek", "deepseek-chat", model.Usage{PromptTokens: 4000, CompletionTokens: 1000, TotalTokens: 5000}, 100); err != nil {
		t.Fatalf("RecordUsageForModel: %v", err)
	}
	if err := b1.RecordUsage(8, model.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("budget file was not written on record: %v", err)
	}

	b2 := NewWithClock(100000, clock)
	n, err := b2.SetFile(path)
	if err != nil {
		t.Fatalf("SetFile (reload) = %v, want nil", err)
	}
	if n != 2 {
		t.Errorf("SetFile restored %d chat(s), want 2", n)
	}
	if got := b2.Usage(7); got != 5000 {
		t.Errorf("reloaded Usage(7) = %d, want 5000", got)
	}
	if got := b2.Usage(8); got != 150 {
		t.Errorf("reloaded Usage(8) = %d, want 150", got)
	}
	byModel := b2.DayUsageByModel(7)
	du, ok := byModel[ModelKey{Provider: "deepseek", Model: "deepseek-chat"}]
	if !ok {
		t.Fatalf("reloaded DayUsageByModel(7) has no deepseek/deepseek-chat bucket: %v", byModel)
	}
	if du.Total != 5000 || du.Prompt != 4000 || du.Completion != 1000 || du.Subagent != 100 {
		t.Errorf("reloaded deepseek bucket = %+v, want {Prompt:4000 Completion:1000 Subagent:100 Total:5000}", du)
	}
	// Recording after a reload keeps accumulating and persists
	// again: a third Budget on the same file sees the new total.
	if err := b2.RecordUsage(7, model.Usage{TotalTokens: 100}, 0); err != nil {
		t.Fatalf("RecordUsage after reload: %v", err)
	}
	b3 := NewWithClock(100000, clock)
	if _, err := b3.SetFile(path); err != nil {
		t.Fatalf("SetFile (second reload) = %v, want nil", err)
	}
	if got := b3.Usage(7); got != 5100 {
		t.Errorf("reloaded Usage(7) after a further record = %d, want 5100", got)
	}
}

// TestW3BBudgetStaleDayLoadsEmpty proves yesterday's spend never
// counts against today: a file from a previous UTC day loads as
// empty instead of resurrecting the old totals.
func TestW3BBudgetStaleDayLoadsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	day := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	b1 := NewWithClock(100000, func() time.Time { return day })
	if _, err := b1.SetFile(path); err != nil {
		t.Fatalf("SetFile: %v", err)
	}
	if err := b1.RecordUsage(7, model.Usage{TotalTokens: 5000}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	nextDay := time.Date(2026, 11, 14, 0, 30, 0, 0, time.UTC)
	b2 := NewWithClock(100000, func() time.Time { return nextDay })
	n, err := b2.SetFile(path)
	if err != nil {
		t.Fatalf("SetFile (next day) = %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("SetFile restored %d chat(s) from a stale day, want 0", n)
	}
	if got := b2.Usage(7); got != 0 {
		t.Errorf("Usage(7) after day rollover = %d, want 0", got)
	}
}

// TestW3BBudgetCorruptFileErrorsButKeepsWorking proves a corrupt
// persistence file is an error (never silently overwritten) while
// the in-memory counters keep working, so a bad file degrades to
// in-memory-only instead of failing the daemon.
func TestW3BBudgetCorruptFileErrorsButKeepsWorking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	b := New(100000)
	if _, err := b.SetFile(path); err == nil {
		t.Error("SetFile (corrupt file) = nil, want an error")
	}
	if err := b.RecordUsage(7, model.Usage{TotalTokens: 10}, 0); err != nil {
		t.Fatalf("RecordUsage after failed SetFile: %v", err)
	}
	if got := b.Usage(7); got != 10 {
		t.Errorf("Usage(7) = %d, want 10 (in-memory accounting must survive a bad file)", got)
	}
}
