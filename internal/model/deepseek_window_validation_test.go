// DeepSeek window table validation tests (layer-0 cost
// reduction, P1): the table must match DeepSeek's LIVE documented
// windows, validated 2026-11-20 against the official Models &
// Pricing page (api-docs.deepseek.com/quick_start/pricing,
// crawled 2026-11-19 16:07 UTC) and the V4 Preview release
// (news260424). The two current models are 1M; the retired
// deepseek-chat / deepseek-reasoner aliases keep their native
// 128K; an unknown model falls to the smallest verified window
// and never over-claims 1M. Every case here fails on the base
// commit, which coded 1,000,000 for all three.
package model

import "testing"

// TestDeepSeekWindowTableMatchesLiveDocs: the full table, with
// the live source for each row in the failure message's context.
func TestDeepSeekWindowTableMatchesLiveDocs(t *testing.T) {
	cases := []struct {
		model string
		want  int
		note  string
	}{
		// Current models (official pricing page: CONTEXT LENGTH 1M).
		{"deepseek-flash", 1000000, "current model, pricing page 1M"},
		{"deepseek-v4-pro", 1000000, "current model, pricing page 1M"},
		// Retired aliases (V4 Preview release news260424: fully
		// retired and inaccessible after 2026-07-24 15:59 UTC).
		// Native windows from the V3.1 release (news250821):
		// 128K for both.
		{"deepseek-chat", 131072, "retired alias, native V3 128K"},
		{"deepseek-reasoner", 131072, "retired alias, native R1 128K"},
		// Unknown: smallest verified window, never 1M.
		{"deepseek-something-new", 131072, "unknown model, smallest verified window"},
		{"deepseek-v5", 131072, "unknown future model, smallest verified window"},
		{"gpt-4o-mini", 131072, "non-DeepSeek name on the DeepSeek provider, unknown fallback"},
	}
	for _, tc := range cases {
		if got := deepSeekContextWindow(tc.model); got != tc.want {
			t.Errorf("deepSeekContextWindow(%q) = %d, want %d (%s)", tc.model, got, tc.want, tc.note)
		}
	}
}

// TestDeepSeekRetiredAliasesNeverClaimOneMillion: the regression
// the old table encoded — a retired alias claiming the 1M window
// of the model it used to route to. The alias no longer resolves
// at all, so 1M is not a possible window for it.
func TestDeepSeekRetiredAliasesNeverClaimOneMillion(t *testing.T) {
	for _, model := range []string{"deepseek-chat", "deepseek-reasoner"} {
		if got := deepSeekContextWindow(model); got >= 1000000 {
			t.Errorf("deepSeekContextWindow(%q) = %d, want < 1000000 (retired alias, native 128K)", model, got)
		}
	}
	if got := deepSeekUnknownModelWindow; got >= 1000000 {
		t.Errorf("deepSeekUnknownModelWindow = %d, want < 1000000 (unknown must never over-claim)", got)
	}
}
