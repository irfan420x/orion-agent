package messaging

import (
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// TestFormatFooterWithCacheHit: a turn the provider reported as
// cached breaks the prompt out — "10,534 tokens (prompt 10,530,
// 10,527 cached)" — the 396-turn shape, where the footer used to
// show only the total and hide that nearly the whole prompt came
// from cache.
func TestFormatFooterWithCacheHit(t *testing.T) {
	turn := &core.TurnStats{
		Provider: "deepseek", Model: "deepseek-flash",
		Tokens: 10534, PromptTokens: 10530, CompletionTokens: 3,
		CacheHitTokens: 10527, CacheReported: true,
		Steps: 1, Duration: 2400 * time.Millisecond,
	}
	want := "— deepseek-flash · 10,534 tokens (prompt 10,530, 10,527 cached) · 1 steps · 2.4s"
	if got := FormatFooter(turn); got != want {
		t.Fatalf("FormatFooter = %q, want %q", got, want)
	}
}

// TestFormatFooterWithoutCacheReport: an unreported split renders
// exactly as before — never "0 cached".
func TestFormatFooterWithoutCacheReport(t *testing.T) {
	turn := &core.TurnStats{
		Provider: "deepseek", Model: "deepseek-flash",
		Tokens: 10534, PromptTokens: 10530, CompletionTokens: 3,
		Steps: 1, Duration: 2400 * time.Millisecond,
	}
	want := "— deepseek-flash · 10,534 tokens · 1 steps · 2.4s"
	if got := FormatFooter(turn); got != want {
		t.Fatalf("FormatFooter = %q, want %q", got, want)
	}
}

// TestFormatFooterWithReportedZeroCacheHit: a provider that measured
// 0 cache hits (first turn, nothing cached yet) also renders as
// before — the footer only breaks out a hit when there is one to
// show.
func TestFormatFooterWithReportedZeroCacheHit(t *testing.T) {
	turn := &core.TurnStats{
		Provider: "deepseek", Model: "deepseek-flash",
		Tokens: 10534, PromptTokens: 10530, CompletionTokens: 3,
		CacheHitTokens: 0, CacheReported: true,
		Steps: 1, Duration: 2400 * time.Millisecond,
	}
	want := "— deepseek-flash · 10,534 tokens · 1 steps · 2.4s"
	if got := FormatFooter(turn); got != want {
		t.Fatalf("FormatFooter = %q, want %q", got, want)
	}
}
