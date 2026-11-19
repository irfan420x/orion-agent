package model

import (
	"testing"
)

func intPtr(n int) *int { return &n }

// TestToModelUsageDeepSeekFields: the DeepSeek top-level
// prompt_cache_hit_tokens / prompt_cache_miss_tokens land on the
// provider-neutral usage with CacheReported set.
func TestToModelUsageDeepSeekFields(t *testing.T) {
	u := toModelUsage(openAIUsage{
		PromptTokens:          10530,
		CompletionTokens:      3,
		TotalTokens:           10533,
		PromptCacheHitTokens:  intPtr(10527),
		PromptCacheMissTokens: intPtr(3),
	})
	if !u.CacheReported {
		t.Fatal("CacheReported must be true when the provider reported a cache split")
	}
	if got := u.CacheHit(); got != 10527 {
		t.Fatalf("CacheHit() = %d, want 10527", got)
	}
	if got := u.CacheMiss(); got != 3 {
		t.Fatalf("CacheMiss() = %d, want 3", got)
	}
	if u.PromptTokens != 10530 || u.CompletionTokens != 3 || u.TotalTokens != 10533 {
		t.Fatalf("counts changed: %+v", u)
	}
}

// TestToModelUsageOpenAIDetailsFallback: OpenAI reports the cache
// hit only as usage.prompt_tokens_details.cached_tokens — that
// must be read as the hit count, with the miss derived.
func TestToModelUsageOpenAIDetailsFallback(t *testing.T) {
	u := openAIUsage{PromptTokens: 1000, CompletionTokens: 10, TotalTokens: 1010}
	u.PromptTokensDetails.CachedTokens = intPtr(800)
	got := toModelUsage(u)
	if !got.CacheReported {
		t.Fatal("CacheReported must be true when prompt_tokens_details.cached_tokens is present")
	}
	if hit := got.CacheHit(); hit != 800 {
		t.Fatalf("CacheHit() = %d, want 800", hit)
	}
	// The miss is derived as prompt − hit by estimateUsage, not
	// invented here: the wire miss stays unset until then.
	if got.PromptCacheMissTokens != 0 {
		t.Fatalf("PromptCacheMissTokens = %d, want 0 (derived later)", got.PromptCacheMissTokens)
	}
}

// TestToModelUsageNoCacheFields: a provider that reports no cache
// fields must read as "unknown", never as "0 cached".
func TestToModelUsageNoCacheFields(t *testing.T) {
	u := toModelUsage(openAIUsage{PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105})
	if u.CacheReported {
		t.Fatal("CacheReported must be false when no cache field arrived")
	}
	if got := u.CacheHit(); got != 0 {
		t.Fatalf("CacheHit() = %d, want 0", got)
	}
	// CacheMiss() derives prompt − hit, which is the whole prompt
	// — but without CacheReported the caller must not display it
	// as a measured miss.
	if got := u.CacheMiss(); got != 100 {
		t.Fatalf("CacheMiss() = %d, want 100 (derived)", got)
	}
}

// TestToModelUsageMissOnly: a provider that reports only the miss
// side still counts as having reported a split (first turn: 0 hits
// is a measurement, not "unknown").
func TestToModelUsageMissOnly(t *testing.T) {
	u := toModelUsage(openAIUsage{
		PromptTokens:          500,
		CompletionTokens:      5,
		TotalTokens:           505,
		PromptCacheMissTokens: intPtr(500),
	})
	if !u.CacheReported {
		t.Fatal("CacheReported must be true when the miss side arrived")
	}
	if got := u.CacheHit(); got != 0 {
		t.Fatalf("CacheHit() = %d, want 0", got)
	}
}

// TestUsageCacheClamping: a malformed provider report (more cached
// than prompt, or negative) can never inflate the cached count or
// price a negative split.
func TestUsageCacheClamping(t *testing.T) {
	u := Usage{PromptTokens: 100, PromptCacheHitTokens: 150}
	if got := u.CacheHit(); got != 100 {
		t.Fatalf("CacheHit() = %d, want 100 (clamped to prompt)", got)
	}
	if got := u.CacheMiss(); got != 0 {
		t.Fatalf("CacheMiss() = %d, want 0", got)
	}
	u = Usage{PromptTokens: 100, PromptCacheHitTokens: -5}
	if got := u.CacheHit(); got != 0 {
		t.Fatalf("CacheHit() = %d, want 0 (negative clamped)", got)
	}
}

// TestEstimateUsageCacheNormalisation: a reported split is
// normalised, never invented — a hit above the prompt is clamped,
// a missing miss is derived as prompt − hit, and an unreported
// split stays unreported.
func TestEstimateUsageCacheNormalisation(t *testing.T) {
	req := ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}
	msg := Message{Role: "assistant", Content: "hello"}

	// Hit above the prompt: clamped, miss derived as 0.
	got := estimateUsage(req, msg, Usage{
		PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105,
		PromptCacheHitTokens: 150, CacheReported: true,
	})
	if got.PromptCacheHitTokens != 100 {
		t.Fatalf("hit = %d, want 100 (clamped)", got.PromptCacheHitTokens)
	}
	if got.PromptCacheMissTokens != 0 {
		t.Fatalf("miss = %d, want 0 (derived)", got.PromptCacheMissTokens)
	}

	// Hit only: miss derived as prompt − hit.
	got = estimateUsage(req, msg, Usage{
		PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105,
		PromptCacheHitTokens: 80, CacheReported: true,
	})
	if got.PromptCacheMissTokens != 20 {
		t.Fatalf("miss = %d, want 20 (derived)", got.PromptCacheMissTokens)
	}

	// No cache data at all: nothing is fabricated.
	got = estimateUsage(req, msg, Usage{PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105})
	if got.CacheReported {
		t.Fatal("CacheReported must stay false when nothing was reported")
	}
	if got.PromptCacheHitTokens != 0 || got.PromptCacheMissTokens != 0 {
		t.Fatalf("unreported split must stay zero, got %+v", got)
	}
}
