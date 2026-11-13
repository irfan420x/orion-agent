// W3b (rc10) regression tests for Audit 5 F-A5-P2-23: tokens the
// provider reports in TotalTokens above the prompt/completion/
// subagent split must be priced, never silently dropped.
package cost

import (
	"testing"

	"github.com/irfan/orion/internal/model"
)

// TestW3BEstimatePricesRemainder proves the remainder is priced at
// the output rate: a provider reporting TotalTokens above the split
// (e.g. cached tokens counted in its total but not in the split)
// has those tokens priced instead of silently unpriced.
func TestW3BEstimatePricesRemainder(t *testing.T) {
	tbl := DefaultTable()
	// deepseek-chat: $0.28/1M in, $0.42/1M out. Split prices
	// 1000 x 0.28 + 500 x 0.42 = $0.00049; the 500-token
	// remainder (2000 - 1000 - 500) adds 500 x 0.42 = $0.00021.
	usd, ok := tbl.Estimate("deepseek", "deepseek-chat", model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 2000}, 0)
	if !ok {
		t.Fatal("Estimate with a remainder = unknown, want a price")
	}
	if want := 0.00070; usd < want-1e-12 || usd > want+1e-12 {
		t.Errorf("Estimate with remainder = $%v, want $%v (split $0.00049 + remainder $0.00021)", usd, want)
	}
	// EstimateTotalsWithTotal agrees with Estimate on the same
	// numbers, so the /usage day-cost path prices remainders too.
	usd2, ok2 := tbl.EstimateTotalsWithTotal("deepseek", "deepseek-chat", 1000, 500, 0, 2000)
	if !ok2 || usd2 != usd {
		t.Errorf("EstimateTotalsWithTotal = $%v/%v, want $%v/true (same as Estimate)", usd2, ok2, usd)
	}
	// No remainder, no extra charge: Total == split prices exactly
	// the split.
	usd3, ok3 := tbl.Estimate("deepseek", "deepseek-chat", model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, 0)
	if !ok3 {
		t.Fatal("Estimate without a remainder = unknown, want a price")
	}
	if want := 0.00049; usd3 < want-1e-12 || usd3 > want+1e-12 {
		t.Errorf("Estimate without remainder = $%v, want $%v", usd3, want)
	}
}
