// Audit 6 regression tests for auto-compaction (compact):
// F-A6-P2-1 (single-shared-token check accepts degenerate
// summaries), F-A6-P2-2 (one giant message defeats the summariser
// input cap), F-A6-P2-3 (summariser prompt ships raw content;
// summary stored unredacted).
package core

import (
	"context"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// fakeSecret is a clearly fake OpenAI-shaped key (unusable shape,
// no real credential): RedactSecrets must catch the sk-... form.
const fakeSecretKey = "sk-fakeSecretKey1234567890"

func TestSummaryRejectsSingleWordRepeated(t *testing.T) {
	// F-A6-P2-1 (audit repro): a summary of the word "project"
	// repeated 200 times used to pass verification ("1 token
	// shared"). It says one thing, not a summary.
	src := []model.Message{
		{Role: "user", Content: "the project plan is due Friday, project budget review next week"},
		{Role: "assistant", Content: "noted the project deadlines"},
	}
	if summaryPreservesSource(strings.Repeat("project ", 200), src) {
		t.Error("a summary of one word repeated 200 times was accepted")
	}
	// The bar is three DISTINCT shared content tokens: a summary
	// sharing only two is still degenerate.
	src2 := []model.Message{
		{Role: "user", Content: "my favourite number is 17"},
		{Role: "assistant", Content: "noted, 17 is your favourite number"},
	}
	if summaryPreservesSource("favourite number", src2) {
		t.Error("a summary sharing only two distinct content tokens was accepted")
	}
	// ...but a genuine one-fact summary still passes: it shares
	// three (favourite, number, 17).
	if !summaryPreservesSource("The user's favourite number is 17.", src2) {
		t.Error("a genuine one-fact summary (3 shared tokens) was rejected")
	}
}

func TestSummaryRejectsRepetitionPaddedSummary(t *testing.T) {
	// F-A6-P2-1, density leg: three shared tokens padded with
	// 100 repeats of filler collapses the information density
	// below the floor, so the summary is refused even though the
	// shared-token count alone would pass.
	src := []model.Message{
		{Role: "user", Content: "my favourite number is 17"},
	}
	summary := "favourite number 17 " + strings.Repeat("blah ", 100)
	if summaryPreservesSource(summary, src) {
		t.Error("a repetition-padded summary (density below the floor) was accepted")
	}
}

func TestBoundedOldPrefixTruncatesGiantMessage(t *testing.T) {
	// F-A6-P2-2 (audit repro): one message of 20,004 tokens used
	// to be handed to the provider even though the cap is 16,000
	// — the cap was per-message, not total, and a single message
	// was always taken whole.
	giant := model.Message{Role: "user", Content: strings.Repeat("word ", 20000)}
	if got := messageTokens(giant); got != 20004 {
		t.Fatalf("test setup: giant message = %d tokens, want 20004", got)
	}
	budget := maxCompactionInputTokens - summariserPromptOverheadTokens
	got := boundedOldPrefix([]model.Message{giant}, budget)
	if len(got) != 1 {
		t.Fatalf("boundedOldPrefix returned %d messages, want 1 (truncated)", len(got))
	}
	if tokens := messageTokens(got[0]); tokens > budget {
		t.Errorf("truncated message = %d tokens, over the %d-token budget", tokens, budget)
	}
	if got[0].Content == "" {
		t.Error("truncated message is empty: the giant turn was dropped, not summarised")
	}
	if !strings.HasPrefix(giant.Content, got[0].Content) {
		t.Error("truncated content is not a prefix of the original")
	}
	// The caller's message is untouched: truncation only bounds
	// what the summariser sees.
	if messageTokens(giant) != 20004 {
		t.Error("boundedOldPrefix mutated the caller's message")
	}
	// A normal prefix still passes through whole and bounded in
	// total, not just per message.
	msgs := []model.Message{
		{Role: "user", Content: strings.Repeat("word ", 9000)},
		{Role: "user", Content: strings.Repeat("word ", 9000)},
	}
	got = boundedOldPrefix(msgs, budget)
	total := 0
	for _, m := range got {
		total += messageTokens(m)
	}
	if total > budget {
		t.Errorf("bounded prefix total = %d tokens, over the %d-token budget", total, budget)
	}
	if len(got) != 1 {
		t.Errorf("bounded prefix = %d messages, want 1 (the second 9k-token message does not fit)", len(got))
	}
}

func TestDefaultCompactionSummariserBoundsTotalInput(t *testing.T) {
	// F-A6-P2-2 at the provider boundary: the whole provider-bound
	// request (system + user prompt, header included) must fit
	// the 16,000-token cap even when a direct caller passes a
	// single 20,004-token message.
	prov := &fakeProvider{chatFn: func(_ int, req model.ChatRequest) (*model.ChatResponse, error) {
		total := 0
		for _, m := range req.Messages {
			total += model.CountTokens(m.Content)
		}
		if total > maxCompactionInputTokens {
			t.Errorf("provider-bound summariser input = %d tokens, over the %d-token cap", total, maxCompactionInputTokens)
		}
		return &model.ChatResponse{
			Message:      model.Message{Role: "assistant", Content: "Summary: the user's favourite number is 17."},
			FinishReason: "stop",
		}, nil
	}}
	agent := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	giant := model.Message{Role: "user", Content: strings.Repeat("word ", 20000)}
	if _, err := agent.defaultCompactionSummariser(context.Background(), []model.Message{giant}); err != nil {
		t.Fatalf("defaultCompactionSummariser: %v", err)
	}
}

func TestDefaultCompactionSummariserRedactsSecrets(t *testing.T) {
	// F-A6-P2-3, provider leg: a fake secret in the transcript
	// must not appear in the provider-bound prompt.
	prov := &fakeProvider{chatFn: func(_ int, req model.ChatRequest) (*model.ChatResponse, error) {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, fakeSecretKey) {
				t.Errorf("provider-bound prompt contains the pasted secret verbatim: %q", truncateRunesForLog(m.Content, 120))
			}
		}
		var prompt string
		for _, m := range req.Messages {
			prompt += m.Content
		}
		if !strings.Contains(prompt, "[REDACTED]") {
			t.Error("provider-bound prompt has no [REDACTED] marker for the pasted secret")
		}
		return &model.ChatResponse{
			Message:      model.Message{Role: "assistant", Content: "Summary: the user's favourite number is 17."},
			FinishReason: "stop",
		}, nil
	}}
	agent := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	_, err := agent.defaultCompactionSummariser(context.Background(), []model.Message{
		{Role: "user", Content: "my favourite number is 17, and here is my key: " + fakeSecretKey},
	})
	if err != nil {
		t.Fatalf("defaultCompactionSummariser: %v", err)
	}
}

func TestAppendSummaryFactRedactsSecrets(t *testing.T) {
	// F-A6-P2-3, stored leg (fact store): the fact store has no
	// scrub of its own, so the summary must be redacted before
	// it is stored as a fact.
	store := newFakeStore()
	if err := appendSummaryFact(store, "The user's favourite number is 17. The user pasted a key "+fakeSecretKey+" in chat."); err != nil {
		t.Fatalf("appendSummaryFact: %v", err)
	}
	fact, found, err := store.GetFact("conversation-summary")
	if err != nil || !found {
		t.Fatalf("conversation-summary fact = %+v, found=%v, err=%v", fact, found, err)
	}
	if strings.Contains(fact.Value, fakeSecretKey) {
		t.Errorf("stored summary fact contains the secret verbatim: %q", fact.Value)
	}
	if !strings.Contains(fact.Value, "[REDACTED]") {
		t.Errorf("stored summary fact has no [REDACTED] marker: %q", fact.Value)
	}
}

func TestAutoCompactRedactsEchoedSecretInStoredSummary(t *testing.T) {
	// F-A6-P2-3, end to end: a summariser that echoes a secret
	// from the turns must not get it into the stored fact or the
	// transcript summary.
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50) // first turn carries the favourite-number fact
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	sink := &fakeTranscriptSink{}
	agent.SetTranscript(sink)
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		return "The user's favourite number is 17. The user pasted a key " + fakeSecretKey + " in chat.", nil
	})
	if _, err := agent.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st := agent.CompactionStatus(); st.Compactions != 1 {
		t.Fatalf("CompactionStatus = %+v, want 1 compaction (the redacted summary still verifies)", st)
	}
	fact, found, err := store.GetFact("conversation-summary")
	if err != nil || !found {
		t.Fatalf("conversation-summary fact = %+v, found=%v, err=%v", fact, found, err)
	}
	if strings.Contains(fact.Value, fakeSecretKey) {
		t.Errorf("stored summary fact contains the secret verbatim: %q", fact.Value)
	}
	_, sums := sink.counts()
	if sums != 1 {
		t.Fatalf("transcript summaries = %d, want 1", sums)
	}
	sink.mu.Lock()
	stored := sink.summaries[0]
	sink.mu.Unlock()
	if strings.Contains(stored, fakeSecretKey) {
		t.Errorf("transcript summary contains the secret verbatim: %q", stored)
	}
}
