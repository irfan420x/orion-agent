package budget

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/irfan/orion/internal/model"
)

// TestRecordUsageKeepsCacheSplit: a reported cache split is tracked
// for the day — inside Prompt, never additional to the meter — so
// the USD estimate can price cached tokens at the cache-hit rate.
func TestRecordUsageKeepsCacheSplit(t *testing.T) {
	b := New(100000)
	// The 396-turn shape: 10,530 prompt, 10,527 of them cache hits.
	u := model.Usage{
		PromptTokens:          10530,
		CompletionTokens:      3,
		TotalTokens:           10533,
		PromptCacheHitTokens:  10527,
		PromptCacheMissTokens: 3,
		CacheReported:         true,
	}
	if err := b.RecordUsageForModel(42, "deepseek", "deepseek-flash", u, 0); err != nil {
		t.Fatalf("RecordUsageForModel = %v, want nil", err)
	}
	// The meter counts the whole turn once — cached tokens are
	// inside Prompt, never added a second time.
	if got := b.Usage(42); got != 10533 {
		t.Errorf("Usage = %d, want 10533 (cache hits must not double-count)", got)
	}
	du := b.DayUsage(42)
	if du.Prompt != 10530 || du.CacheHit != 10527 {
		t.Errorf("DayUsage = %+v, want {Prompt:10530 CacheHit:10527}", du)
	}
	byModel := b.DayUsageByModel(42)
	md, ok := byModel[ModelKey{Provider: "deepseek", Model: "deepseek-flash"}]
	if !ok {
		t.Fatal("DayUsageByModel has no deepseek/deepseek-flash bucket")
	}
	if md.CacheHit != 10527 || md.Prompt != 10530 {
		t.Errorf("per-model DayUsage = %+v, want {Prompt:10530 CacheHit:10527}", md)
	}
	// A second turn without a cache report adds prompt but no hit.
	if err := b.RecordUsageForModel(42, "deepseek", "deepseek-flash", model.Usage{
		PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105,
	}, 0); err != nil {
		t.Fatalf("second RecordUsageForModel = %v, want nil", err)
	}
	du = b.DayUsage(42)
	if du.Prompt != 10630 || du.CacheHit != 10527 || du.Total != 10638 {
		t.Errorf("DayUsage after second turn = %+v, want {Prompt:10630 CacheHit:10527 Total:10638}", du)
	}
}

// TestRecordUsageCacheHitWithoutReportIsIgnored: a hit count with
// CacheReported false is not a measurement — it must not enter the
// day's cache total.
func TestRecordUsageCacheHitWithoutReportIsIgnored(t *testing.T) {
	b := New(100000)
	u := model.Usage{PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105, PromptCacheHitTokens: 90}
	if err := b.RecordUsage(7, u, 0); err != nil {
		t.Fatalf("RecordUsage = %v, want nil", err)
	}
	if got := b.DayUsage(7).CacheHit; got != 0 {
		t.Errorf("DayUsage.CacheHit = %d, want 0 (unreported hit is not a measurement)", got)
	}
}

// TestRecordUsageCacheHitClamped: a malformed hit above the prompt
// records at most the whole prompt as cached.
func TestRecordUsageCacheHitClamped(t *testing.T) {
	b := New(100000)
	u := model.Usage{PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105, PromptCacheHitTokens: 500, CacheReported: true}
	if err := b.RecordUsage(7, u, 0); err != nil {
		t.Fatalf("RecordUsage = %v, want nil", err)
	}
	if got := b.DayUsage(7).CacheHit; got != 100 {
		t.Errorf("DayUsage.CacheHit = %d, want 100 (clamped to prompt)", got)
	}
}

// TestBudgetPersistenceKeepsCacheSplit: the cache split survives a
// daemon restart through the persistence file, and a file written
// before the cache field existed loads as 0 cached (not an error).
func TestBudgetPersistenceKeepsCacheSplit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	b := New(100000)
	if _, err := b.SetFile(path); err != nil {
		t.Fatalf("SetFile = %v, want nil", err)
	}
	u := model.Usage{
		PromptTokens:         10530,
		CompletionTokens:     3,
		TotalTokens:          10533,
		PromptCacheHitTokens: 10527,
		CacheReported:        true,
	}
	if err := b.RecordUsageForModel(42, "deepseek", "deepseek-flash", u, 0); err != nil {
		t.Fatalf("RecordUsageForModel = %v, want nil", err)
	}
	b2 := New(100000)
	if _, err := b2.SetFile(path); err != nil {
		t.Fatalf("reload SetFile = %v, want nil", err)
	}
	if du := b2.DayUsage(42); du.CacheHit != 10527 || du.Prompt != 10530 {
		t.Errorf("reloaded DayUsage = %+v, want {Prompt:10530 CacheHit:10527}", du)
	}

	// A pre-cache-field file (no cache_hit keys at all) loads
	// cleanly with 0 cached.
	legacy := `{"version":1,"day":"` + b2.today() + `","chats":{"7":{"used":150,"prompt":100,"completion":50,"subagent":0,"models":[{"provider":"deepseek","model":"deepseek-flash","used":150,"prompt":100,"completion":50,"subagent":0}]}}}`
	legacyPath := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("writing legacy file: %v", err)
	}
	b3 := New(100000)
	if _, err := b3.SetFile(legacyPath); err != nil {
		t.Fatalf("legacy SetFile = %v, want nil", err)
	}
	if du := b3.DayUsage(7); du.CacheHit != 0 || du.Prompt != 100 {
		t.Errorf("legacy DayUsage = %+v, want {Prompt:100 CacheHit:0}", du)
	}
}
