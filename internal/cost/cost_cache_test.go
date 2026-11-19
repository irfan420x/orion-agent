package cost

import (
	"math"
	"testing"

	"github.com/irfan/orion/internal/model"
)

// TestEstimatePricesCacheHitAtCacheHitRate: the 396-turn shape —
// 10,530 prompt tokens, 10,527 of them cache hits, 3 completion —
// prices the hit tokens at the deepseek-flash cache-hit rate
// ($0.003/1M), not the $0.15/1M miss rate. Before this fix the
// same turn priced the whole prompt as a miss, ~47x too high.
func TestEstimatePricesCacheHitAtCacheHitRate(t *testing.T) {
	tbl := DefaultTable()
	u := model.Usage{
		PromptTokens:         10530,
		CompletionTokens:     3,
		TotalTokens:          10533,
		PromptCacheHitTokens: 10527,
		CacheReported:        true,
	}
	got, ok := tbl.Estimate("deepseek", "deepseek-flash", u, 0)
	if !ok {
		t.Fatal("Estimate returned not-ok for a known model")
	}
	// 10527 * 0.003/1e6 + 3 * 0.15/1e6 + 3 * 0.60/1e6.
	want := 10527*0.003/1_000_000.0 + 3*0.15/1_000_000.0 + 3*0.60/1_000_000.0
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("Estimate = %v, want %v", got, want)
	}
	// Sanity: the cached turn must cost far less than the same
	// turn priced as a full miss.
	miss, ok := tbl.Estimate("deepseek", "deepseek-flash", model.Usage{
		PromptTokens: 10530, CompletionTokens: 3, TotalTokens: 10533,
	}, 0)
	if !ok {
		t.Fatal("Estimate (miss) returned not-ok")
	}
	if got*10 >= miss {
		t.Fatalf("cached estimate %v is not < 1/10 of the miss estimate %v", got, miss)
	}
}

// TestEstimateUnreportedCachePricesAsMiss: without a provider
// report, every prompt token prices at the input rate — the old
// behaviour, unchanged.
func TestEstimateUnreportedCachePricesAsMiss(t *testing.T) {
	tbl := DefaultTable()
	got, ok := tbl.Estimate("deepseek", "deepseek-flash", model.Usage{
		PromptTokens: 1000, CompletionTokens: 10, TotalTokens: 1010,
	}, 0)
	if !ok {
		t.Fatal("Estimate returned not-ok for a known model")
	}
	want := 1000*0.15/1_000_000.0 + 10*0.60/1_000_000.0
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("Estimate = %v, want %v", got, want)
	}
}

// TestEstimateCacheHitRateUnknownFallsBack: a model with no
// cache-hit rate in the table prices a reported hit at the input
// rate — never a discount the table does not know.
func TestEstimateCacheHitRateUnknownFallsBack(t *testing.T) {
	tbl := &Table{Version: "test", Effective: "2026-11-19", entries: map[string]map[string]Price{
		"custom": {"m": {InputPerMillion: 1.00, OutputPerMillion: 2.00}},
	}}
	got, ok := tbl.Estimate("custom", "m", model.Usage{
		PromptTokens:         1000,
		CompletionTokens:     10,
		TotalTokens:          1010,
		PromptCacheHitTokens: 900,
		CacheReported:        true,
	}, 0)
	if !ok {
		t.Fatal("Estimate returned not-ok for a known model")
	}
	want := 1000*1.00/1_000_000.0 + 10*2.00/1_000_000.0
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("Estimate = %v, want %v (all prompt at input rate)", got, want)
	}
}

// TestEstimateCacheHitClampedToPrompt: a malformed hit above the
// prompt prices at most the whole prompt as cached.
func TestEstimateCacheHitClampedToPrompt(t *testing.T) {
	tbl := DefaultTable()
	got, ok := tbl.Estimate("openai", "gpt-4o", model.Usage{
		PromptTokens:         100,
		CompletionTokens:     0,
		TotalTokens:          100,
		PromptCacheHitTokens: 500,
		CacheReported:        true,
	}, 0)
	if !ok {
		t.Fatal("Estimate returned not-ok for a known model")
	}
	// All 100 prompt tokens at the $1.25/1M cache-hit rate.
	want := 100 * 1.25 / 1_000_000.0
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("Estimate = %v, want %v", got, want)
	}
}

// TestEstimateAnthropicCacheReadRate: an Anthropic cache read
// prices at 10% of the input rate.
func TestEstimateAnthropicCacheReadRate(t *testing.T) {
	tbl := DefaultTable()
	got, ok := tbl.Estimate("anthropic", "claude-sonnet-4", model.Usage{
		PromptTokens:         1000,
		CompletionTokens:     10,
		TotalTokens:          1010,
		PromptCacheHitTokens: 800,
		CacheReported:        true,
	}, 0)
	if !ok {
		t.Fatal("Estimate returned not-ok for a known model")
	}
	want := 800*0.30/1_000_000.0 + 200*3.00/1_000_000.0 + 10*15.00/1_000_000.0
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("Estimate = %v, want %v", got, want)
	}
}
