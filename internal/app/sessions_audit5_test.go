// Audit 5 regression tests at the app layer: F-A5-1 (FIFO cap
// drops, end to end over the real FileStore + wired transcript)
// and F-A5-P2-5 (resume drops summaries, silently caps, never
// reports corrupt lines).
package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/session"
)

// seedChatHistory fills store with n alternating user/assistant
// turns of ~words tokens each. The first user turn carries the
// fact the summary must preserve.
func seedChatHistory(t *testing.T, store memory.Store, n, words int) {
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

func TestAppCompactionStatusReportsDroppedTurns(t *testing.T) {
	// F-A5-1 end to end: 195 turns in the chat's real FileStore,
	// summariser always errors -> breaker trips -> 3 more turns.
	// The loss must be visible in CompactionStatus, in /status,
	// and in the transcript's spill records — not silent.
	dir := t.TempDir()
	a := testAppOnDir(t, dir)
	chatID := int64(42)
	ctx := context.Background()

	agent, err := a.AgentForChat(chatID)
	if err != nil {
		t.Fatalf("AgentForChat: %v", err)
	}
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	seedChatHistory(t, cs.store, 195, 5)
	agent.SetCompactionSummariser(func(ctx context.Context, old []model.Message) (string, error) {
		return "", errors.New("provider down")
	})
	for i := 0; i < 6; i++ {
		if _, err := agent.Run(ctx, "turn"); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	msgs := chatMessages(t, a, chatID)
	if len(msgs) != memory.MaxMessages {
		t.Errorf("history = %d, want %d (pinned at the cap)", len(msgs), memory.MaxMessages)
	}
	if containsText(msgs, "my favourite number is 17") {
		t.Error("seeded first turn still in history: it should have been dropped by the cap")
	}
	if _, found, _ := cs.store.GetFact("conversation-summary"); found {
		t.Error("a summary fact exists, but the summariser never succeeded")
	}
	// 6 turns x 2 messages = 12 added to 195 = 207; the cap
	// drops 7.
	st := agent.CompactionStatus()
	if st.DroppedWithoutSummary != 7 {
		t.Errorf("agent DroppedWithoutSummary = %d, want 7", st.DroppedWithoutSummary)
	}
	appSt, err := a.CompactionStatusForChat(chatID)
	if err != nil {
		t.Fatalf("CompactionStatusForChat: %v", err)
	}
	if appSt.DroppedWithoutSummary != 7 {
		t.Errorf("app DroppedWithoutSummary = %d, want 7", appSt.DroppedWithoutSummary)
	}
	if status := a.StatusLineForChat(chatID); !strings.Contains(status, "dropped without summary") {
		t.Errorf("/status does not name the dropped turns:\n%s", status)
	}
	recs, err := cs.transcript.Records()
	if err != nil {
		t.Fatalf("transcript Records: %v", err)
	}
	dropped := 0
	preserved := false
	for _, r := range recs {
		if r.Kind == session.KindDropped {
			dropped++
			if strings.Contains(r.Text, "favourite number is 17") {
				preserved = true
			}
		}
	}
	if dropped != 7 {
		t.Errorf("transcript dropped records = %d, want 7", dropped)
	}
	if !preserved {
		t.Error("no spill record preserves the dropped first turn")
	}
}

func TestResumeLoadsSummariesReportsCorruptAndCap(t *testing.T) {
	// F-A5-P2-5: resume of a transcript holding 250 messages, a
	// compaction summary and a corrupt line must (a) load the
	// summary into the shared fact, (b) report the 200-cap, and
	// (c) report the corrupt line — none of it silent.
	dir := t.TempDir()
	a := testAppOnDir(t, dir)
	chatID := int64(9)
	ctx := context.Background()

	agent, err := a.AgentForChat(chatID)
	if err != nil {
		t.Fatalf("AgentForChat: %v", err)
	}
	if _, err := agent.Run(ctx, "live session turn"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}

	// Build the target session's transcript directly: 250 turns,
	// one summary, one corrupt line.
	targetID := session.NewID()
	tr, err := session.Open(cs.dir, targetID)
	if err != nil {
		t.Fatalf("session.Open target: %v", err)
	}
	for i := 0; i < 250; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := tr.AppendMessage(model.Message{Role: role, Content: "old session turn"}); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}
	if err := tr.AppendSummary("the user's favourite number is 17, discussed in the old session"); err != nil {
		t.Fatalf("AppendSummary: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(cs.dir, "sessions", targetID+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open target for corrupt write: %v", err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatalf("write corrupt line: %v", err)
	}
	f.Close()
	if err := tr.Close(); err != nil {
		t.Fatalf("Close target: %v", err)
	}

	resolved, n, err := a.ResumeSessionForChat(chatID, targetID)
	if err != nil {
		t.Fatalf("ResumeSessionForChat: %v", err)
	}
	if resolved != targetID {
		t.Fatalf("resumed id=%q, want %q", resolved, targetID)
	}
	// The returned count is the working history: capped at 200.
	if n != memory.MaxMessages {
		t.Errorf("resumed message count=%d, want %d (capped working history)", n, memory.MaxMessages)
	}
	if got := chatMessages(t, a, chatID); len(got) != memory.MaxMessages {
		t.Fatalf("history after resume = %d messages, want %d", len(got), memory.MaxMessages)
	}
	rep, ok := a.LastResumeReportForChat(chatID)
	if !ok {
		t.Fatal("no resume report recorded")
	}
	if rep.TranscriptMessages != 250 || rep.LoadedMessages != 200 || !rep.Capped {
		t.Errorf("resume report = %+v, want 250 transcript / 200 loaded / capped", rep)
	}
	if rep.CorruptLines != 1 {
		t.Errorf("resume report corrupt lines = %d, want 1", rep.CorruptLines)
	}
	if rep.SummariesLoaded != 1 {
		t.Errorf("resume report summaries loaded = %d, want 1", rep.SummariesLoaded)
	}
	// The transcript's summary reached the shared fact, so the
	// next turn's recall can still surface it.
	fact, found, err := cs.store.GetFact("conversation-summary")
	if err != nil || !found || !strings.Contains(fact.Value, "favourite number is 17") {
		t.Errorf("summary fact after resume: found=%v value=%q err=%v", found, fact.Value, err)
	}
	// Resuming the same session again does not duplicate the
	// summary in the fact.
	if _, _, err := a.ResumeSessionForChat(chatID, targetID); err != nil {
		t.Fatalf("resume live: %v", err)
	}
	fact, _, _ = cs.store.GetFact("conversation-summary")
	if n := strings.Count(fact.Value, "favourite number is 17"); n != 1 {
		t.Errorf("summary fact mentions the number %d times, want 1 (no duplication on re-resume)", n)
	}
}
