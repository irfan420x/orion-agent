// Audit 5 regression tests for auto-compaction (compact +
// transcripts): F-A5-1 (FIFO cap still silently drops history),
// F-A5-2 (summary content never verified), F-A5-P2-1 (transcript
// summary-append failure swallowed), F-A5-P2-2 (stale pressure),
// F-A5-P2-7 (breaker never re-arms, warning band skipped).
package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/session"
	"github.com/irfan/orion/internal/tools"
)

// seedStoreHistory fills store with n alternating user/assistant
// turns of ~words tokens each, using a real cap-enforcing store
// (EphemeralStore/FileStore) — never fakeStore, whose AddMessage
// has no 200-cap and so cannot reproduce the compaction/FIFO
// interaction at all (Audit 5 F-A5-1). The first user turn carries
// the fact the summary must preserve.
func seedStoreHistory(t *testing.T, store memory.Store, n, words int) {
	t.Helper()
	for i := 0; i < n; i++ {
		role := "user"
		content := strings.TrimSpace(strings.Repeat("word ", words))
		if i == 0 {
			content = "my favourite number is 17 " + strings.Repeat("word ", words)
		}
		if i%2 == 1 {
			role = "assistant"
			content = strings.TrimSpace(strings.Repeat("word ", words))
		}
		if err := store.AddMessage(model.Message{Role: role, Content: content}); err != nil {
			t.Fatalf("seed AddMessage: %v", err)
		}
	}
}

func TestAutoCompactRejectsContentFreeSummary(t *testing.T) {
	// F-A5-2: a provider returning filler must refuse the
	// compaction and leave history untouched — the prefix is not
	// dropped on the word of a summary that preserves nothing.
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50)
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		return "Nothing durable was discussed.", nil
	})
	if _, err := agent.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(store.allMessages()); got != 30+2 {
		t.Errorf("history = %d, want 32: a content-free summary must not drop anything", got)
	}
	if _, found, _ := store.GetFact("conversation-summary"); found {
		t.Error("content-free summary was stored as a fact")
	}
	st := agent.CompactionStatus()
	if st.Compactions != 0 || st.ConsecutiveFailures != 1 {
		t.Errorf("CompactionStatus = %+v, want 0 compactions, 1 failure", st)
	}
}

func TestSummaryPreservesSource(t *testing.T) {
	src := []model.Message{
		{Role: "user", Content: "my favourite number is 17"},
		{Role: "assistant", Content: "noted, 17 is your favourite number"},
	}
	if !summaryPreservesSource("The user's favourite number is 17.", src) {
		t.Error("a summary preserving the seeded fact was rejected")
	}
	if summaryPreservesSource("Nothing durable was discussed.", src) {
		t.Error("a content-free summary was accepted")
	}
	if summaryPreservesSource("x", src) {
		t.Error("a one-rune summary was accepted")
	}
	if summaryPreservesSource("The user's favourite number is 17.", nil) {
		t.Error("a summary of an empty source was accepted")
	}
}

func TestAutoCompactSpillsDroppedTurnsWhenBreakerTripped(t *testing.T) {
	// F-A5-1 (audit repro shape): 195 turns in a real
	// cap-enforcing store, summariser always errors -> breaker
	// trips after 3 calls -> 3 more turns. The old outcome was:
	// history pinned at 200, the seeded first turn gone, and NO
	// conversation-summary fact — silent loss. The new outcome:
	// every dropped turn is counted, logged and spilled to the
	// transcript as an explicit "dropped, unsummarised" record.
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := memory.NewEphemeralStore()
	seedStoreHistory(t, store, 195, 5)
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	tr, err := session.Open(t.TempDir(), session.NewID())
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	agent.SetTranscript(tr)
	calls := 0
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		calls++
		return "", errors.New("provider down")
	})
	for i := 0; i < 6; i++ {
		if _, err := agent.Run(context.Background(), "turn"); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if calls != 3 {
		t.Errorf("summariser called %d times, want 3 (then tripped, no retry)", calls)
	}
	msgs, err := store.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != memory.MaxMessages {
		t.Errorf("history = %d, want %d (pinned at the cap)", len(msgs), memory.MaxMessages)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "favourite number is 17") {
			t.Error("seeded first turn still in history: it should have been dropped by the cap")
		}
	}
	if _, found, _ := store.GetFact("conversation-summary"); found {
		t.Error("a summary fact exists, but the summariser never succeeded")
	}
	st := agent.CompactionStatus()
	if !st.Tripped {
		t.Error("breaker not tripped after 3 failures")
	}
	// 6 turns x 2 messages = 12 added to 195 = 207; the cap
	// drops 7.
	if st.DroppedWithoutSummary != 7 {
		t.Errorf("DroppedWithoutSummary = %d, want 7", st.DroppedWithoutSummary)
	}
	if st.LastDroppedAt.IsZero() {
		t.Error("LastDroppedAt is zero after drops")
	}
	recs, err := tr.Records()
	if err != nil {
		t.Fatalf("transcript Records: %v", err)
	}
	dropped := 0
	for _, r := range recs {
		if r.Kind == session.KindDropped {
			dropped++
			if !strings.Contains(r.Text, "Dropped without summary") {
				t.Errorf("dropped record text = %q, want the spill marker", r.Text)
			}
		}
	}
	if dropped != 7 {
		t.Errorf("transcript dropped records = %d, want 7", dropped)
	}
	// The seeded first turn survives in the spill record, not
	// just in the count.
	found := false
	for _, r := range recs {
		if r.Kind == session.KindDropped && strings.Contains(r.Text, "favourite number is 17") {
			found = true
		}
	}
	if !found {
		t.Error("no spill record preserves the dropped first turn")
	}
}

func TestAutoCompactEmergencySummarisesFarOverBudgetHistory(t *testing.T) {
	// F-A5-1 (audit repro shape, ~39x pressure): the summariser
	// used to be called 0 times and the prefix left for the FIFO
	// cap. Now the oldest turns are summarised in a bounded slice
	// and the prefix is chipped away.
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := memory.NewEphemeralStore()
	seedStoreHistory(t, store, 30, 1300) // ~30 x 1304 tokens: ~38x
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	var summarised []model.Message
	agent.SetCompactionSummariser(func(_ context.Context, old []model.Message) (string, error) {
		summarised = old
		return "Summary of the dropped turns: the user's favourite number is 17, discussed across many wordy turns, with details about the project plan and the next steps the user outlined.", nil
	})
	if _, err := agent.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(summarised) == 0 {
		t.Fatal("summariser never called at ~38x pressure")
	}
	if len(summarised) >= 20 {
		t.Errorf("summariser got %d messages, want a bounded slice of the 20-message prefix", len(summarised))
	}
	tokens := 0
	for _, m := range summarised {
		tokens += messageTokens(m)
	}
	if tokens > maxCompactionInputTokens {
		t.Errorf("summariser got %d tokens, over the %d-token input cap", tokens, maxCompactionInputTokens)
	}
	st := agent.CompactionStatus()
	if st.Compactions != 1 {
		t.Errorf("CompactionStatus = %+v, want 1 emergency compaction", st)
	}
	if fact, found, _ := store.GetFact("conversation-summary"); !found || !strings.Contains(fact.Value, "17") {
		t.Errorf("summary fact = %+v, found=%v; want the favourite number preserved", fact, found)
	}
	msgs, err := store.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if got := len(msgs); got != 30-len(summarised)+2 {
		t.Errorf("history = %d, want %d (seed minus summarised slice plus the new turn)", got, 30-len(summarised)+2)
	}
}

// failingSummarySink is a transcript sink whose summary append
// always fails, to prove a transcript failure aborts the
// compaction (F-A5-P2-1).
type failingSummarySink struct {
	fakeTranscriptSink
	summaryErr error
}

func (s *failingSummarySink) AppendSummary(text string) error {
	return s.summaryErr
}

func TestRecordSummaryFailureAbortsCompaction(t *testing.T) {
	// F-A5-P2-1: the transcript is the unbounded backstop — if
	// its summary append fails, the drop must not proceed.
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50)
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	sink := &failingSummarySink{summaryErr: errors.New("disk full")}
	agent.SetTranscript(sink)
	agent.SetCompactionSummariser(func(_ context.Context, old []model.Message) (string, error) {
		return "The user's favourite number is 17.", nil
	})
	if _, err := agent.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(store.allMessages()); got != 30+2 {
		t.Errorf("history = %d, want 32: a failed transcript append must not drop anything", got)
	}
	if _, sums := sink.counts(); sums != 0 {
		t.Errorf("transcript summaries = %d, want 0 (the append failed)", sums)
	}
	st := agent.CompactionStatus()
	if st.Compactions != 0 || st.ConsecutiveFailures != 1 {
		t.Errorf("CompactionStatus = %+v, want 0 compactions, 1 failure", st)
	}
}

func TestCompactionStatusPressureRecomputedAfterCompaction(t *testing.T) {
	// F-A5-P2-2: /status and the footer must report the history
	// that is actually kept now, not the pre-compaction pressure.
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50) // pressure ~1.6 before compaction
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		return "The user's favourite number is 17.", nil
	})
	if _, err := agent.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := agent.CompactionStatus()
	if st.Compactions != 1 {
		t.Fatalf("CompactionStatus = %+v, want 1 compaction", st)
	}
	// Within a small tolerance of the live value: the stored
	// pressure is computed on the kept history at compaction
	// time, and the turn's own two messages raise it slightly
	// afterwards. The bug was the stale pre-compaction value
	// (~1.59 for a history that is actually ~6% full).
	want := HistoryPressure(store.allMessages(), 1024)
	if diff := st.Pressure - want; diff < -0.05 || diff > 0.05 {
		t.Errorf("Pressure = %.2f, want %.2f (recomputed on the kept history)", st.Pressure, want)
	}
	if st.Pressure >= AutoCompactThreshold {
		t.Errorf("Pressure = %.2f after a successful compaction, want it below the %.2f trigger", st.Pressure, AutoCompactThreshold)
	}
}

func TestBreakerRearmsAfterCooldown(t *testing.T) {
	// F-A5-P2-7: a transient provider outage tripping the breaker
	// must not disable auto-compaction for the rest of the
	// session — after the cooldown it re-arms and retries.
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
	for i := 0; i < 4; i++ {
		if _, err := agent.Run(context.Background(), "turn"); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if calls != 3 {
		t.Fatalf("summariser called %d times, want 3 (then tripped)", calls)
	}
	if st := agent.CompactionStatus(); !st.Tripped {
		t.Fatalf("breaker not tripped: %+v", st)
	}
	// Cooldown elapsed (simulated by moving trippedAt back) and
	// the provider recovered: the next turn retries.
	c := agent.compactorOrDefault()
	c.mu.Lock()
	c.trippedAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		calls++
		return "Recovered summary: the user's favourite number is 17.", nil
	})
	if _, err := agent.Run(context.Background(), "turn after outage"); err != nil {
		t.Fatalf("Run after cooldown: %v", err)
	}
	if calls != 4 {
		t.Errorf("summariser called %d times, want 4 (one retry after re-arm)", calls)
	}
	st := agent.CompactionStatus()
	if st.Tripped || st.Compactions != 1 {
		t.Errorf("CompactionStatus after re-arm = %+v, want untripped, 1 compaction", st)
	}
}

func TestPressureJumpOverWarningBandWarns(t *testing.T) {
	// F-A5-P2-7: a single turn jumping pressure from below the
	// warning band to the trigger still warns — the user gets the
	// advance notice even though the band was never entered —
	// and compaction still proceeds.
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50) // pressure ~1.6, previous 0
	agent := newTestAgent(t, cfg, &fakeProvider{}, tools.NewRegistry(), store)
	agent.SetCompactionSummariser(func(_ context.Context, _ []model.Message) (string, error) {
		return "The user's favourite number is 17.", nil
	})
	var warnings int
	agent.SetEventHandler(func(ev ProgressEvent) {
		if ev.Kind == EventCompactionWarning {
			warnings++
		}
	})
	all, err := store.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if !agent.maybeAutoCompact(context.Background(), all) {
		t.Fatal("maybeAutoCompact did not compact on a jump over the band")
	}
	if warnings != 1 {
		t.Errorf("compaction warnings = %d, want 1 (the jump warning)", warnings)
	}
	if st := agent.CompactionStatus(); st.Compactions != 1 {
		t.Errorf("CompactionStatus = %+v, want 1 compaction despite the jump", st)
	}
}
