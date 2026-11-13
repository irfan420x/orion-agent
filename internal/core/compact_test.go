package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// fakeTranscriptSink records what the agent appends to a transcript.
type fakeTranscriptSink struct {
	mu        sync.Mutex
	messages  []model.Message
	summaries []string
}

func (s *fakeTranscriptSink) AppendMessage(msg model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
	return nil
}

func (s *fakeTranscriptSink) AppendSummary(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summaries = append(s.summaries, text)
	return nil
}

func (s *fakeTranscriptSink) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages), len(s.summaries)
}

// wordy returns a message of roughly n tokens: CountTokens charges
// about one token per short word, plus 4 for framing.
func wordy(role string, words int) model.Message {
	return model.Message{Role: role, Content: strings.TrimSpace(strings.Repeat("word ", words))}
}

// seedHistory fills store with n alternating user/assistant turns
// of ~words tokens each. The first user turn carries the fact the
// summary must preserve.
func seedHistory(t *testing.T, store *fakeStore, n, words int) {
	t.Helper()
	for i := 0; i < n; i++ {
		role := "user"
		content := wordy("user", words).Content
		if i == 0 {
			content = "my favourite number is 17 " + strings.Repeat("word ", words)
		}
		if i%2 == 1 {
			role = "assistant"
			content = wordy("assistant", words).Content
		}
		if err := store.AddMessage(model.Message{Role: role, Content: content}); err != nil {
			t.Fatalf("seed AddMessage: %v", err)
		}
	}
}

func TestHistoryPressure(t *testing.T) {
	// Token leg: 10 messages x ~54 tokens = ~540 tokens over a
	// 1024 budget is ~0.53; the count leg (10/200) is far below.
	msgs := make([]model.Message, 10)
	for i := range msgs {
		msgs[i] = wordy("user", 50)
	}
	p := HistoryPressure(msgs, 1024)
	if p < 0.5 || p > 0.6 {
		t.Errorf("HistoryPressure token leg = %.2f, want ~0.53", p)
	}
	// Count leg: 180 short messages over a huge budget still
	// reads 0.90 from the count alone.
	msgs = make([]model.Message, 180)
	for i := range msgs {
		msgs[i] = model.Message{Role: "user", Content: "hi"}
	}
	if p := HistoryPressure(msgs, 1000000); p != 0.90 {
		t.Errorf("HistoryPressure count leg = %.2f, want 0.90", p)
	}
	// Zero budget disables the token leg; empty history is 0.
	if p := HistoryPressure(msgs, 0); p != 0.90 {
		t.Errorf("HistoryPressure zero budget = %.2f, want 0.90", p)
	}
	if p := HistoryPressure(nil, 1024); p != 0 {
		t.Errorf("HistoryPressure empty = %.2f, want 0", p)
	}
}

func TestAutoCompactFiresAtThresholdAndPreservesFact(t *testing.T) {
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50) // ~30 x 54 tokens: pressure >> 0.8
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	sink := &fakeTranscriptSink{}
	agent.SetTranscript(sink)
	summarised := 0
	agent.SetCompactionSummariser(func(_ context.Context, old []model.Message) (string, error) {
		summarised++
		// The summariser must preserve the seeded fact: the
		// test asserts it survives in the stored summary.
		for _, m := range old {
			if strings.Contains(m.Content, "favourite number is 17") {
				return "The user's favourite number is 17.", nil
			}
		}
		return "Nothing durable was discussed.", nil
	})

	reply, err := agent.Run(context.Background(), "what is my favourite number?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply == "" {
		t.Error("Run returned an empty reply")
	}
	if summarised != 1 {
		t.Errorf("summariser called %d times, want 1", summarised)
	}
	// The summary fact holds the seeded fact, so the next turn's
	// recall can still surface it.
	fact, found, err := store.GetFact("conversation-summary")
	if err != nil || !found {
		t.Fatalf("conversation-summary fact = %+v, found=%v, err=%v", fact, found, err)
	}
	if !strings.Contains(fact.Value, "17") {
		t.Errorf("summary fact = %q, want it to preserve the favourite number 17", fact.Value)
	}
	// Working history is compacted: the dropped prefix is gone,
	// the recent window plus the new turn survives.
	msgs := store.allMessages()
	if len(msgs) > AutoCompactKeepRecent+2 {
		t.Errorf("history after auto-compact = %d messages, want at most %d", len(msgs), AutoCompactKeepRecent+2)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "favourite number is 17") {
			t.Errorf("dropped prefix still in working history: %q", m.Content)
		}
	}
	if !strings.Contains(msgs[len(msgs)-1].Content, "canned answer") {
		t.Errorf("last message = %+v, want the new turn's answer", msgs[len(msgs)-1])
	}
	// The transcript got the summary AND the new turn's messages
	// (seeded turns predate the transcript wiring, so they are not
	// re-recorded here — migration covers those at open time).
	transcriptMsgs, transcriptSums := sink.counts()
	if transcriptSums != 1 {
		t.Errorf("transcript summaries = %d, want 1", transcriptSums)
	}
	if transcriptMsgs != 2 {
		t.Errorf("transcript messages = %d, want 2 (the new turn)", transcriptMsgs)
	}
	st := agent.CompactionStatus()
	if st.Compactions != 1 || st.Tripped || st.ConsecutiveFailures != 0 {
		t.Errorf("CompactionStatus = %+v, want 1 success, untripped", st)
	}
}

func TestAutoCompactWarningBandDoesNotCompact(t *testing.T) {
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 14, 50) // ~14 x 54 = ~756 tokens: ~0.74
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	summarised := 0
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		summarised++
		return "summary", nil
	})
	var warnings int
	handler := func(ev ProgressEvent) {
		if ev.Kind == EventCompactionWarning {
			warnings++
		}
	}
	before := len(store.allMessages())
	if _, err := agent.RunWithEventHandler(context.Background(), "hello", handler); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summarised != 0 {
		t.Errorf("summariser called %d times in the warning band, want 0", summarised)
	}
	if warnings != 1 {
		t.Errorf("compaction warnings = %d, want 1", warnings)
	}
	after := len(store.allMessages())
	if after != before+2 {
		t.Errorf("history changed in the warning band: %d -> %d, want +%d (the new turn only)", before, after, 2)
	}
	if _, found, _ := store.GetFact("conversation-summary"); found {
		t.Error("warning band wrote a summary fact")
	}
	st := agent.CompactionStatus()
	if !st.Warning || st.NeedsCompact {
		t.Errorf("CompactionStatus = %+v, want Warning=true, NeedsCompact=false", st)
	}
}

func TestAutoCompactCircuitBreaker(t *testing.T) {
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50)
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	calls := 0
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		calls++
		return "", errors.New("provider down")
	})
	// Three consecutive failures trip the breaker; the fourth
	// turn must not even call the summariser.
	for i := 0; i < 4; i++ {
		if _, err := agent.Run(context.Background(), fmt.Sprintf("turn %d", i)); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if calls != 3 {
		t.Errorf("summariser called %d times, want 3 (then tripped, no retry)", calls)
	}
	st := agent.CompactionStatus()
	if !st.Tripped || st.ConsecutiveFailures != 3 {
		t.Errorf("CompactionStatus = %+v, want Tripped with 3 failures", st)
	}
	if st.LastError == "" {
		t.Error("CompactionStatus.LastError empty after failures")
	}
	// History is untouched by the failed compactions (only the
	// four turns' own messages were added).
	if got := len(store.allMessages()); got != 30+8 {
		t.Errorf("history = %d messages, want %d (seed + 4 turns, nothing dropped)", got, 30+8)
	}
	// Reset re-arms: the next turn retries the summariser.
	agent.ResetCompactionBreaker()
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		return "Recovered summary: the user's favourite number is 17.", nil
	})
	if _, err := agent.Run(context.Background(), "turn 5"); err != nil {
		t.Fatalf("Run after reset: %v", err)
	}
	if calls != 3 {
		t.Errorf("summariser calls changed unexpectedly: %d", calls)
	}
	st = agent.CompactionStatus()
	if st.Tripped || st.Compactions != 1 {
		t.Errorf("CompactionStatus after reset+success = %+v, want untripped, 1 compaction", st)
	}
}

func TestAutoCompactEmergencySummarisesOverBudgetHistory(t *testing.T) {
	// A history many times the budget (legacy store, or a single
	// giant tool result) is no longer skipped entirely (Audit 5
	// F-A5-1): the oldest turns are summarised in a BOUNDED slice
	// (maxCompactionInputTokens), so the summariser never receives
	// a prompt over the provider context, and the prefix is
	// chipped away instead of left for the FIFO cap to drop
	// silently. The request trim is no longer the only safety net.
	// (This test replaces TestAutoCompactSkipsSeverelyOverBudgetHistory,
	// which asserted the old skip.)
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 500) // ~30 x 500 tokens: pressure ~15x
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	var summarised []model.Message
	agent.SetCompactionSummariser(func(_ context.Context, old []model.Message) (string, error) {
		summarised = old
		return "Summary of the dropped turns: the user's favourite number is 17, discussed across many wordy turns.", nil
	})
	if _, err := agent.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(summarised) == 0 {
		t.Fatal("summariser never called on a severely over-budget history: the prefix is left for the FIFO cap to drop silently")
	}
	// Bounded: the summarised slice must fit the input cap.
	tokens := 0
	for _, m := range summarised {
		tokens += messageTokens(m)
	}
	if tokens > maxCompactionInputTokens {
		t.Errorf("summariser got %d tokens, over the %d-token input cap", tokens, maxCompactionInputTokens)
	}
	// The seeded fact survived in the summary, and only the
	// summarised slice (not the whole prefix) was dropped.
	if fact, found, _ := store.GetFact("conversation-summary"); !found || !strings.Contains(fact.Value, "17") {
		t.Errorf("summary fact = %+v, found=%v; want the favourite number preserved", fact, found)
	}
	st := agent.CompactionStatus()
	if st.Compactions != 1 {
		t.Errorf("CompactionStatus = %+v, want 1 emergency compaction", st)
	}
	if got := len(store.allMessages()); got != 30-len(summarised)+2 {
		t.Errorf("history = %d, want %d (seed minus summarised slice plus the new turn)", got, 30-len(summarised)+2)
	}
}

func TestAutoCompactEmptySummaryFailsWithoutDropping(t *testing.T) {
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50)
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		return "   ", nil // empty after trim: not a summary
	})
	if _, err := agent.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(store.allMessages()); got != 30+2 {
		t.Errorf("history = %d, want 32: an empty summary must not drop anything", got)
	}
	if st := agent.CompactionStatus(); st.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", st.ConsecutiveFailures)
	}
}

func TestRecordMessageWritesTranscript(t *testing.T) {
	agent := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	sink := &fakeTranscriptSink{}
	agent.SetTranscript(sink)
	if _, err := agent.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs, sums := sink.counts()
	if msgs != 2 || sums != 0 {
		t.Errorf("transcript got %d messages + %d summaries, want 2 + 0", msgs, sums)
	}
	// Unwiring the transcript stops recording without affecting
	// the turn.
	agent.SetTranscript(nil)
	if _, err := agent.Run(context.Background(), "again"); err != nil {
		t.Fatalf("Run unwired: %v", err)
	}
	if msgs, _ := sink.counts(); msgs != 2 {
		t.Errorf("transcript got %d messages after unwire, want 2", msgs)
	}
}

func TestAppendSummaryFactAccumulatesAndCaps(t *testing.T) {
	store := newFakeStore()
	if err := appendSummaryFact(store, "First summary: the user's favourite number is 17."); err != nil {
		t.Fatalf("appendSummaryFact 1: %v", err)
	}
	if err := appendSummaryFact(store, "Second summary: the user prefers short replies."); err != nil {
		t.Fatalf("appendSummaryFact 2: %v", err)
	}
	fact, _, _ := store.GetFact("conversation-summary")
	if !strings.Contains(fact.Value, "17") || !strings.Contains(fact.Value, "short replies") {
		t.Errorf("summary fact = %q, want both sections", fact.Value)
	}
	// Past the cap, the oldest section is dropped and the newest
	// survives.
	big := strings.Repeat("x", maxSummaryFactRunes)
	if err := appendSummaryFact(store, big); err != nil {
		t.Fatalf("appendSummaryFact big: %v", err)
	}
	fact, _, _ = store.GetFact("conversation-summary")
	if strings.Contains(fact.Value, "favourite number") {
		t.Error("oldest summary section survived past the cap")
	}
	if !strings.Contains(fact.Value, big) {
		t.Error("newest summary section lost past the cap")
	}
	if n := len([]rune(fact.Value)); n > maxSummaryFactRunes+len([]rune("## 2006-01-02 15:04:05 UTC\n")) {
		t.Errorf("summary fact = %d runes, want it bounded near %d", n, maxSummaryFactRunes)
	}
	// A manual /compress summary (no header) is kept as the
	// oldest section, not mangled.
	store2 := newFakeStore()
	if err := store2.AddFact("conversation-summary", "Manual summary: project Orion."); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if err := appendSummaryFact(store2, "Auto summary: the user's favourite number is 17."); err != nil {
		t.Fatalf("appendSummaryFact over manual: %v", err)
	}
	fact, _, _ = store2.GetFact("conversation-summary")
	if !strings.Contains(fact.Value, "Manual summary") || !strings.Contains(fact.Value, "Auto summary") {
		t.Errorf("summary fact over manual = %q, want both", fact.Value)
	}
}

func TestDefaultCompactionSummariserUsesProvider(t *testing.T) {
	prov := &fakeProvider{chatFn: func(_ int, req model.ChatRequest) (*model.ChatResponse, error) {
		if len(req.Messages) < 2 || !strings.Contains(req.Messages[0].Content, "summarise") {
			return nil, fmt.Errorf("unexpected summariser prompt: %+v", req.Messages)
		}
		return &model.ChatResponse{
			Message:      model.Message{Role: "assistant", Content: "Summary: the user's favourite number is 17."},
			FinishReason: "stop",
		}, nil
	}}
	agent := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	summary, err := agent.defaultCompactionSummariser(context.Background(), []model.Message{
		{Role: "user", Content: "my favourite number is 17"},
	})
	if err != nil {
		t.Fatalf("defaultCompactionSummariser: %v", err)
	}
	if !strings.Contains(summary, "17") {
		t.Errorf("summary = %q, want the favourite number", summary)
	}
}

func TestHistoryPressureUsesCountBeforeCap(t *testing.T) {
	// 160 short messages: count pressure 0.80 trips compaction
	// even though the tokens are tiny — this is what stops the
	// 200-message FIFO cap from silently dropping a turn.
	msgs := make([]model.Message, 160)
	for i := range msgs {
		msgs[i] = model.Message{Role: "user", Content: "hi"}
	}
	if p := HistoryPressure(msgs, 32768); p < 0.80 {
		t.Errorf("HistoryPressure(160 short) = %.2f, want >= 0.80", p)
	}
}
