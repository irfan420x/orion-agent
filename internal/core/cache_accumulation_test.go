// Prompt-cache accumulation tests: the per-step and side-channel
// cache splits must reach the TurnFinished usage — summed, never
// double-counted, never dropped.
package core

import (
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// TestTurnFinishedCarriesCacheSplit: a main-loop response reporting
// a cache split must have that split on the TurnFinished usage —
// the 396-turn shape, where the provider served 10,527 of 10,530
// prompt tokens from cache.
func TestTurnFinishedCarriesCacheSplit(t *testing.T) {
	prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponseForTest("396", model.Usage{
			PromptTokens:          10530,
			CompletionTokens:      3,
			TotalTokens:           10533,
			PromptCacheHitTokens:  10527,
			PromptCacheMissTokens: 3,
			CacheReported:         true,
		}), nil
	}}
	agent := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())

	usage := turnFinishedUsage(t, agent, "17*23+5")
	if !usage.CacheReported {
		t.Fatal("TurnFinished Usage.CacheReported must be true")
	}
	if got := usage.CacheHit(); got != 10527 {
		t.Fatalf("CacheHit() = %d, want 10527", got)
	}
	if got := usage.CacheMiss(); got != 3 {
		t.Fatalf("CacheMiss() = %d, want 3", got)
	}
	if usage.PromptTokens != 10530 || usage.TotalTokens != 10533 {
		t.Fatalf("counts changed: %+v", usage)
	}
}

// TestTurnFinishedFoldsSideChannelCacheSplit: a side-channel query
// that hit the cache (90 of 100 prompt tokens) must fold its split
// into the turn's usage alongside the main loop's unreported usage.
func TestTurnFinishedFoldsSideChannelCacheSplit(t *testing.T) {
	prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponseForTest("done", model.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}), nil
	}}
	agent := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	agent.AddSideChannelUsage(model.Usage{
		PromptTokens:          100,
		CompletionTokens:      10,
		TotalTokens:           110,
		PromptCacheHitTokens:  90,
		PromptCacheMissTokens: 10,
		CacheReported:         true,
	})

	usage := turnFinishedUsage(t, agent, "hello")
	if usage.TotalTokens != 135 || usage.PromptTokens != 120 || usage.CompletionTokens != 15 {
		t.Fatalf("TurnFinished Usage = %+v, want {Prompt:120 Completion:15 Total:135}", usage)
	}
	if !usage.CacheReported {
		t.Fatal("TurnFinished Usage.CacheReported must be true after a cached side query")
	}
	if got := usage.CacheHit(); got != 90 {
		t.Fatalf("CacheHit() = %d, want 90 (side query only)", got)
	}
}

// TestAddSideChannelUsageAccumulatesCacheSplit: two cached side
// queries sum their hits, and the accumulator resets after one
// take (no double-count across turns).
func TestAddSideChannelUsageAccumulatesCacheSplit(t *testing.T) {
	agent := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	agent.AddSideChannelUsage(model.Usage{PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105, PromptCacheHitTokens: 60, CacheReported: true})
	agent.AddSideChannelUsage(model.Usage{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55, PromptCacheHitTokens: 40, CacheReported: true})
	got := agent.takeSideChannelUsage()
	if got.CacheHit() != 100 || got.PromptTokens != 150 {
		t.Fatalf("takeSideChannelUsage = %+v, want hit 100 of prompt 150", got)
	}
	if again := agent.takeSideChannelUsage(); again.TotalTokens != 0 || again.CacheHit() != 0 {
		t.Fatalf("second take = %+v, want zeroed (no double-count)", again)
	}
}
