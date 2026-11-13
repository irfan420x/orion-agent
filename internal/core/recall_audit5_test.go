// Audit 5 regression tests for recall of the conversation-summary
// fact (F-A5-2): even a good summary barely reached the model —
// recall truncated it to 200 runes per item, max 3 items.
package core

import (
	"strings"
	"testing"

	"github.com/irfan/orion/internal/memory"
)

func TestRecallInjectsLatestSummarySectionWithRaisedCap(t *testing.T) {
	oldSection := "## 2026-11-13 10:00:00 UTC\n" + strings.Repeat("oldtopic alpha filler ", 60)
	latestSection := "## 2026-11-13 11:00:00 UTC\n" + "The user decided newtopic beta is the priority. " + strings.Repeat("newtopic beta detail ", 60)
	fact := memory.Fact{
		Key:   "conversation-summary",
		Value: oldSection + "\n\n" + latestSection,
	}
	items := recallItems("what is newtopic beta?", []memory.Fact{fact}, nil, nil)
	if len(items) != 1 {
		t.Fatalf("recall items = %d, want 1 (the latest summary section)", len(items))
	}
	got := items[0]
	if !strings.Contains(got, "newtopic beta") {
		t.Errorf("injected summary = %q, want the latest section about newtopic beta", got)
	}
	if strings.Contains(got, "oldtopic alpha") {
		t.Errorf("injected summary contains the OLD section: %q", got)
	}
	// Raised cap: the injected section is far longer than the
	// 200-rune item cap that used to hide all but a fragment.
	if n := len([]rune(got)); n <= maxRecallItemRunes {
		t.Errorf("injected summary = %d runes, want more than the %d-rune item cap", n, maxRecallItemRunes)
	}
	if n := len([]rune(got)); n > maxSummaryRecallRunes {
		t.Errorf("injected summary = %d runes, want at most the %d-rune summary cap", n, maxSummaryRecallRunes)
	}
}

func TestRecallSummarySectionDoesNotDisplaceOtherFacts(t *testing.T) {
	// The summary section gets its own slot: three precisely
	// matching facts still come first, and the summary is a
	// fourth item rather than crowding one of them out.
	summary := memory.Fact{
		Key:   "conversation-summary",
		Value: "## 2026-11-13 11:00:00 UTC\nThe user decided newtopic beta is the priority.",
	}
	facts := []memory.Fact{
		{Key: "a", Value: "newtopic gamma"},
		{Key: "b", Value: "newtopic delta"},
		{Key: "c", Value: "newtopic epsilon"},
		summary,
	}
	items := recallItems("what is newtopic beta?", facts, nil, nil)
	if len(items) != 4 {
		t.Fatalf("recall items = %d, want 4 (3 facts + the summary section)", len(items))
	}
	if !strings.Contains(items[3], "newtopic beta") {
		t.Errorf("last item = %q, want the summary section", items[3])
	}
}

func TestRecallSummarySectionNeedsTopicalMatch(t *testing.T) {
	// The raised cap is not a free pass: with no shared content
	// token the summary stays out, as before.
	fact := memory.Fact{
		Key:   "conversation-summary",
		Value: "## 2026-11-13 11:00:00 UTC\nThe user decided newtopic beta is the priority.",
	}
	if items := recallItems("what is the weather today?", []memory.Fact{fact}, nil, nil); len(items) != 0 {
		t.Errorf("recall items = %v for an unrelated query, want none", items)
	}
}
