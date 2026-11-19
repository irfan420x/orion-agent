package app

import (
	"strings"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/cost"
	"github.com/irfan/orion/internal/model"
)

// TestUsageLineForChatWithCacheHit: a day whose turns reported a
// cache split shows the cached tokens on the /usage line, and the
// estimated cost prices the hit tokens at the cache-hit rate — the
// 396-turn shape must no longer price 10,527 cached tokens as a
// $0.15/1M miss.
func TestUsageLineForChatWithCacheHit(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Config.DefaultProvider = "deepseek"
	a.Budget = budget.New(100000)
	u := model.Usage{
		PromptTokens:         10530,
		CompletionTokens:     3,
		TotalTokens:          10533,
		PromptCacheHitTokens: 10527,
		CacheReported:        true,
	}
	if err := a.Budget.RecordUsageForModel(111, "deepseek", "deepseek-flash", u, 0); err != nil {
		t.Fatalf("RecordUsageForModel: %v", err)
	}
	got := a.UsageLineForChat(111)
	for _, want := range []string{
		"Tokens today (this chat): 10,533 (10,530 prompt, 10,527 cached + 3 completion, incl. 0 subagent)",
		// 10527 * $0.003 + 3 * $0.15 + 3 * $0.60 per 1M ≈ $0.0000338 → $0.0000 at four decimals.
		"Est. cost today (this chat): $0.0000 (est., deepseek-flash, price table v2026-11-19 effective 2026-11-19)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("UsageLineForChat =\n%s\nmissing %q", got, want)
		}
	}
}

// TestEstimateDayCostWithCacheHit prices one cached bucket at the
// split rates, and an unreported bucket at the full miss rate.
func TestEstimateDayCostWithCacheHit(t *testing.T) {
	tbl := cost.DefaultTable()
	byModel := map[budget.ModelKey]budget.DayUsage{
		{Provider: "deepseek", Model: "deepseek-flash"}: {
			Prompt: 10530, Completion: 3, Subagent: 0, Total: 10533, CacheHit: 10527,
		},
	}
	got, _, ok := estimateDayCost(tbl, byModel, "deepseek", "deepseek-flash")
	if !ok {
		t.Fatal("estimateDayCost returned not-ok for a known model")
	}
	want := 10527*0.003/1_000_000.0 + 3*0.15/1_000_000.0 + 3*0.60/1_000_000.0
	if got != want {
		t.Fatalf("estimateDayCost = %v, want %v", got, want)
	}
	// Same bucket with no cache report: the whole prompt at the
	// miss rate.
	byModel[budget.ModelKey{Provider: "deepseek", Model: "deepseek-flash"}] = budget.DayUsage{
		Prompt: 10530, Completion: 3, Subagent: 0, Total: 10533, CacheHit: 0,
	}
	got, _, ok = estimateDayCost(tbl, byModel, "deepseek", "deepseek-flash")
	if !ok {
		t.Fatal("estimateDayCost (miss) returned not-ok")
	}
	want = 10530*0.15/1_000_000.0 + 3*0.60/1_000_000.0
	if got != want {
		t.Fatalf("estimateDayCost (miss) = %v, want %v", got, want)
	}
}
