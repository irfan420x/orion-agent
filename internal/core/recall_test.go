package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// --- Unit tests for the scorer -------------------------------------------

func TestRecallItemsRanksRelevantFactFirst(t *testing.T) {
	facts := []memory.Fact{
		{Key: "favourite colour", Value: "blue"},
		{Key: "capital of France", Value: "Paris"},
		{Key: "favourite number", Value: "17"},
	}
	got := recallItems("What is my favourite number?", facts, nil, nil)
	if len(got) == 0 {
		t.Fatal("recallItems: got no items, want the favourite-number fact")
	}
	if got[0] != "favourite number: 17" {
		t.Errorf("recallItems[0] = %q, want %q", got[0], "favourite number: 17")
	}
	// "favourite colour: blue" shares the token "favourite", so it
	// scores 1 and may also be injected (facts need only >= 1) — but
	// below the 2-token match. The zero-overlap fact must not appear.
	for _, item := range got {
		if strings.Contains(item, "Paris") {
			t.Errorf("recallItems injected a zero-overlap fact: %q", item)
		}
	}
}

func TestRecallItemsGreetingScoresNothing(t *testing.T) {
	facts := []memory.Fact{
		{Key: "favourite number", Value: "17"},
		{Key: "favourite colour", Value: "blue"},
	}
	for _, q := range []string{"Hello", "Hello, how are you?", "hi there"} {
		if got := recallItems(q, facts, nil, nil); len(got) != 0 {
			t.Errorf("recallItems(%q) = %v, want nothing injected", q, got)
		}
	}
	// Same for older message sentences: a greeting must not drag in
	// an old statement.
	older := []model.Message{{Role: "user", Content: "My favourite number is 17."}}
	if got := recallItems("Hello", nil, older, nil); len(got) != 0 {
		t.Errorf("recallItems(greeting, older messages) = %v, want nothing", got)
	}
}

// TestRecallItemsThresholds: one shared token injects an explicit fact
// (deliberate, score >= 1) but not a message sentence (noisy,
// score >= 2).
func TestRecallItemsThresholds(t *testing.T) {
	facts := []memory.Fact{{Key: "favourite number", Value: "17"}}
	older := []model.Message{{Role: "user", Content: "My favourite number is 17."}}

	// "What is the number?" shares only "number" with both candidates.
	if got := recallItems("What is the number?", facts, nil, nil); len(got) != 1 || got[0] != "favourite number: 17" {
		t.Errorf("recallItems(one shared token, fact) = %v, want [favourite number: 17]", got)
	}
	if got := recallItems("What is the number?", nil, older, nil); len(got) != 0 {
		t.Errorf("recallItems(one shared token, sentence) = %v, want nothing (needs >= 2)", got)
	}
	// Two shared tokens inject the sentence.
	if got := recallItems("What is my favourite number?", nil, older, nil); len(got) != 1 || !strings.Contains(got[0], "17") {
		t.Errorf("recallItems(two shared tokens, sentence) = %v, want the 17 sentence", got)
	}
}

func TestRecallItemsDropsQuestionsAndInstructions(t *testing.T) {
	older := []model.Message{{Role: "user", Content: "Use the calculator tool to compute my favourite number times 3. What is my favourite number? My favourite number is 17."}}
	got := recallItems("What is my favourite number?", nil, older, nil)
	if len(got) != 1 {
		t.Fatalf("recallItems = %v, want exactly the declarative sentence", got)
	}
	if !strings.Contains(got[0], "17") {
		t.Errorf("recallItems[0] = %q, want the sentence stating 17", got[0])
	}
	if strings.Contains(got[0], "calculator") {
		t.Errorf("recallItems injected the instruction sentence: %q", got[0])
	}

	// A message that is only an instruction yields nothing, even
	// though its tokens overlap the query.
	onlyInstruction := []model.Message{{Role: "user", Content: "Use the calculator tool to compute my favourite number times 3."}}
	if got := recallItems("What is my favourite number?", nil, onlyInstruction, nil); len(got) != 0 {
		t.Errorf("recallItems(instruction only) = %v, want nothing", got)
	}
}

func TestRecallItemsFactsBeforeSentencesOnTie(t *testing.T) {
	facts := []memory.Fact{{Key: "favourite number", Value: "17"}}
	older := []model.Message{{Role: "user", Content: "My favourite number is 17."}}
	got := recallItems("What is my favourite number?", facts, older, nil)
	if len(got) != 2 {
		t.Fatalf("recallItems = %v, want fact + sentence", got)
	}
	if got[0] != "favourite number: 17" {
		t.Errorf("recallItems[0] = %q, want the fact first on a score tie", got[0])
	}
	if !strings.Contains(got[1], "My favourite number is 17") {
		t.Errorf("recallItems[1] = %q, want the message sentence second", got[1])
	}
}

func TestRecallItemsMostRecentSentenceFirst(t *testing.T) {
	older := []model.Message{
		{Role: "user", Content: "My favourite number is 17."},
		{Role: "user", Content: "My favourite number is 23."},
	}
	got := recallItems("What is my favourite number?", nil, older, nil)
	if len(got) != 2 {
		t.Fatalf("recallItems = %v, want both sentences", got)
	}
	if !strings.Contains(got[0], "23") {
		t.Errorf("recallItems[0] = %q, want the most recent sentence first", got[0])
	}
}

func TestRecallItemsCapsAtThree(t *testing.T) {
	var facts []memory.Fact
	for i := 1; i <= 5; i++ {
		facts = append(facts, memory.Fact{Key: fmt.Sprintf("number %d", i), Value: fmt.Sprintf("%d", i)})
	}
	got := recallItems("What is the number?", facts, nil, nil)
	if len(got) != 3 {
		t.Errorf("recallItems = %d items, want cap of 3: %v", len(got), got)
	}
}

func TestRecallItemsSkipsAlreadyVisible(t *testing.T) {
	facts := []memory.Fact{{Key: "favourite number", Value: "17"}}
	recent := []model.Message{{Role: "user", Content: "My favourite number is 17."}}
	if got := recallItems("What is my favourite number?", facts, nil, recent); len(got) != 0 {
		t.Errorf("recallItems(fact already in window) = %v, want nothing", got)
	}
	older := []model.Message{{Role: "user", Content: "My favourite number is 17."}}
	if got := recallItems("What is my favourite number?", nil, older, recent); len(got) != 0 {
		t.Errorf("recallItems(sentence already in window) = %v, want nothing", got)
	}
	// Token containment, not substring: a recent "2017" must not
	// hide the fact value "17".
	recentYear := []model.Message{{Role: "user", Content: "I was born in 2017."}}
	if got := recallItems("What is my favourite number?", facts, nil, recentYear); len(got) != 1 {
		t.Errorf("recallItems(recent 2017) = %v, want the 17 fact (2017 is not 17)", got)
	}
}

func TestRecallItemsTruncatesLongItems(t *testing.T) {
	facts := []memory.Fact{{Key: "note", Value: strings.Repeat("x", 300)}}
	got := recallItems("note", facts, nil, nil)
	if len(got) != 1 {
		t.Fatalf("recallItems = %v, want one item", got)
	}
	if len([]rune(got[0])) != maxRecallItemRunes {
		t.Errorf("injected item is %d runes, want exactly %d", len([]rune(got[0])), maxRecallItemRunes)
	}
}

func TestRecallItemsIgnoresAssistantMessages(t *testing.T) {
	// Only user messages are mined; an assistant echoing the fact
	// back must not become a recall candidate.
	older := []model.Message{{Role: "assistant", Content: "Your favourite number is 17."}}
	if got := recallItems("What is my favourite number?", nil, older, nil); len(got) != 0 {
		t.Errorf("recallItems(assistant message) = %v, want nothing", got)
	}
}

func TestFormatRecallSection(t *testing.T) {
	if got := formatRecallSection(nil); got != "" {
		t.Errorf("formatRecallSection(nil) = %q, want empty", got)
	}
	got := formatRecallSection([]string{"favourite number: 17", "My favourite number is 17."})
	if !strings.HasPrefix(got, "\n\nRelevant things the user told you earlier") {
		t.Errorf("formatRecallSection missing header: %q", got)
	}
	if !strings.Contains(got, "- favourite number: 17\n- My favourite number is 17.") {
		t.Errorf("formatRecallSection items not formatted as bullet lines: %q", got)
	}
}

// --- Agent-level tests (mock provider captures what it received) ------

// systemMessageOf returns the system message the provider received on
// its most recent Chat call.
func systemMessageOf(t *testing.T, prov *fakeProvider) string {
	t.Helper()
	msgs := prov.lastReq.Messages
	if len(msgs) == 0 || msgs[0].Role != "system" {
		t.Fatalf("provider received no system message first: %+v", msgs)
	}
	return msgs[0].Content
}

// runFillerTurns runs n throwaway turns so earlier turns slide out of
// the 10-message recent window. Each turn adds 2 messages (user +
// assistant), so 6 turns push a first turn's 2 messages out.
func runFillerTurns(t *testing.T, a *Agent, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := a.Run(context.Background(), fmt.Sprintf("filler %d", i)); err != nil {
			t.Fatalf("filler turn %d: %v", i, err)
		}
	}
}

func TestRunRecallsStoredFactOutsideWindow(t *testing.T) {
	prov := &fakeProvider{}
	mem := newFakeStore()
	if err := mem.AddFact("favourite number", "17"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	// 6 filler turns = 12 messages; the fact was never in a message,
	// and the window only holds the last 10.
	runFillerTurns(t, a, 6)

	if _, err := a.Run(context.Background(), "What is my favourite number?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sys := systemMessageOf(t, prov)
	if !strings.Contains(sys, "Relevant things the user told you earlier") {
		t.Errorf("system message has no recall section: %q", sys)
	}
	if !strings.Contains(sys, "17") {
		t.Errorf("system message does not contain the recalled fact value 17: %q", sys)
	}
	if !strings.Contains(sys, "favourite number: 17") {
		t.Errorf("system message does not contain the fact as %q: %q", "favourite number: 17", sys)
	}
}

// TestRunRecallsOlderMessageOutsideWindow: the loop never calls
// AddFact, so a fact stated in conversation lives only in old
// messages. After enough filler turns to push the stating turn out
// of the 10-message window, recall must still surface it from the
// older message history.
func TestRunRecallsOlderMessageOutsideWindow(t *testing.T) {
	prov := &fakeProvider{}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	if _, err := a.Run(context.Background(), "My favourite number is 17."); err != nil {
		t.Fatalf("Run(stating turn): %v", err)
	}
	runFillerTurns(t, a, 6)

	if _, err := a.Run(context.Background(), "What is my favourite number?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sys := systemMessageOf(t, prov)
	if !strings.Contains(sys, "Relevant things the user told you earlier") {
		t.Errorf("system message has no recall section: %q", sys)
	}
	if !strings.Contains(sys, "17") {
		t.Errorf("system message does not contain the recalled 17 from the old message: %q", sys)
	}
}

func TestRunRecallsNothingForGreeting(t *testing.T) {
	prov := &fakeProvider{}
	mem := newFakeStore()
	if err := mem.AddFact("favourite number", "17"); err != nil {
		t.Fatal(err)
	}
	if err := mem.AddFact("favourite colour", "blue"); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	if _, err := a.Run(context.Background(), "Hello, how are you?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sys := systemMessageOf(t, prov)
	if sys != a.SystemPrompt() {
		t.Errorf("greeting run changed the system prompt:\n%q\nwant byte-identical system prompt", sys)
	}
	if strings.Contains(sys, "Relevant things the user told you earlier") {
		t.Errorf("greeting run injected a recall section: %q", sys)
	}
}

func TestRunDoesNotDuplicateFactInRecentWindow(t *testing.T) {
	prov := &fakeProvider{}
	mem := newFakeStore()
	if err := mem.AddFact("favourite number", "17"); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	// Turn 1 states the fact; it is not in the window yet, so recall
	// may inject it here.
	if _, err := a.Run(context.Background(), "My favourite number is 17."); err != nil {
		t.Fatalf("Run(turn 1): %v", err)
	}
	if sys := systemMessageOf(t, prov); !strings.Contains(sys, "17") {
		t.Errorf("turn 1 system message should recall the not-yet-visible fact: %q", sys)
	}

	// Turn 2 asks about it while turn 1 is still in the recent
	// window: the fact must NOT be injected again.
	if _, err := a.Run(context.Background(), "What is my favourite number?"); err != nil {
		t.Fatalf("Run(turn 2): %v", err)
	}
	sys := systemMessageOf(t, prov)
	if sys != a.SystemPrompt() {
		t.Errorf("turn 2 system prompt was changed (fact already in window):\n%q\nwant byte-identical system prompt", sys)
	}
}
