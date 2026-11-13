// Per-session transcripts + auto-compaction integration at the app
// layer (audit P0 #3, #4): transcript wiring on agent build,
// rotation on /new, list/resume round-trips, migration of
// pre-transcript history, restart survival, and the manual
// /compress circuit-breaker reset.
package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
)

// TestChatTranscriptWiredAndMigratesLegacyHistory verifies that
// building a chat's agent opens its live session transcript, that
// turns already in the chat's memory.json (pre-transcript history)
// are imported once into it, and that later turns are appended
// there — while memory.json itself is left untouched.
func TestChatTranscriptWiredAndMigratesLegacyHistory(t *testing.T) {
	dir := t.TempDir()
	a := testAppOnDir(t, dir)
	chatID := int64(42)
	ctx := context.Background()

	// Seed pre-transcript history before the agent (and its
	// transcript) exists.
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	seeded := []model.Message{
		{Role: "user", Content: "my favourite number is 17"},
		{Role: "assistant", Content: "noted"},
		{Role: "user", Content: "hello"},
	}
	for _, m := range seeded {
		if err := cs.store.AddMessage(m); err != nil {
			t.Fatalf("seed AddMessage: %v", err)
		}
	}

	agent, err := a.AgentForChat(chatID)
	if err != nil {
		t.Fatalf("AgentForChat: %v", err)
	}
	if cs.sessionID == "" || cs.transcript == nil {
		t.Fatalf("chat transcript not wired: sessionID=%q transcript=%v", cs.sessionID, cs.transcript)
	}
	path := filepath.Join(dir, "chats", "42", "sessions", cs.sessionID+".jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("transcript file %s missing: %v", path, err)
	}
	// The seeded turns were imported once, marked migrated.
	recs, err := cs.transcript.Records()
	if err != nil {
		t.Fatalf("transcript Records: %v", err)
	}
	if len(recs) != len(seeded) {
		t.Fatalf("transcript records=%d, want %d (migrated seed)", len(recs), len(seeded))
	}
	for i, r := range recs {
		if !r.Migrated || r.Message == nil || r.Message.Content != seeded[i].Content {
			t.Fatalf("record %d: migrated=%v content=%q, want migrated seed %q", i, r.Migrated, r.Message, seeded[i].Content)
		}
		if r.Timestamp.IsZero() {
			t.Fatalf("record %d has no timestamp", i)
		}
	}
	// memory.json still holds the seeded turns: migration copies,
	// never deletes.
	if msgs, err := cs.store.RecentMessages(0); err != nil || len(msgs) != len(seeded) {
		t.Fatalf("memory.json after migration: %d messages err=%v, want %d", len(msgs), err, len(seeded))
	}

	// A turn appends to the transcript (user + assistant).
	if _, err := agent.Run(ctx, "what is 2+2?"); err != nil {
		t.Fatalf("agent Run: %v", err)
	}
	msgs, err := cs.transcript.Messages()
	if err != nil {
		t.Fatalf("transcript Messages: %v", err)
	}
	if len(msgs) != len(seeded)+2 {
		t.Fatalf("transcript messages=%d, want %d", len(msgs), len(seeded)+2)
	}
	if !containsText(msgs, "my favourite number is 17") {
		t.Fatalf("transcript lost the seeded fact turn: %v", contents(msgs))
	}
	if !containsText(msgs, "what is 2+2?") {
		t.Fatalf("transcript missing the new user turn: %v", contents(msgs))
	}
}

// TestNewSessionForChatRotatesTranscript verifies /new rotates the
// live session: a fresh id, empty working history, the old
// transcript file left on disk, and the new session listed as live.
func TestNewSessionForChatRotatesTranscript(t *testing.T) {
	dir := t.TempDir()
	a := testAppOnDir(t, dir)
	chatID := int64(7)
	ctx := context.Background()

	agent, err := a.AgentForChat(chatID)
	if err != nil {
		t.Fatalf("AgentForChat: %v", err)
	}
	if _, err := agent.Run(ctx, "first session turn"); err != nil {
		t.Fatalf("agent Run: %v", err)
	}
	oldID, err := a.CurrentSessionIDForChat(chatID)
	if err != nil || oldID == "" {
		t.Fatalf("CurrentSessionIDForChat: id=%q err=%v", oldID, err)
	}
	oldPath := filepath.Join(dir, "chats", "7", "sessions", oldID+".jsonl")
	oldCount := 0
	if cs, err := a.chatSessionFor(chatID); err == nil && cs.transcript != nil {
		oldCount = cs.transcript.Count()
	}
	if oldCount < 2 {
		t.Fatalf("old transcript count=%d, want >= 2", oldCount)
	}

	if err := a.NewSessionForChat(chatID); err != nil {
		t.Fatalf("NewSessionForChat: %v", err)
	}
	newID, err := a.CurrentSessionIDForChat(chatID)
	if err != nil || newID == "" || newID == oldID {
		t.Fatalf("after /new: newID=%q oldID=%q err=%v", newID, oldID, err)
	}
	if msgs := chatMessages(t, a, chatID); len(msgs) != 0 {
		t.Fatalf("history after /new: %d messages, want 0", len(msgs))
	}
	// The old transcript survives rotation, byte for byte.
	if st, err := os.Stat(oldPath); err != nil || st.Size() == 0 {
		t.Fatalf("old transcript %s missing/empty after rotation: %v", oldPath, err)
	}
	infos, live, err := a.ListSessionsForChat(chatID)
	if err != nil {
		t.Fatalf("ListSessionsForChat: %v", err)
	}
	if live != newID || len(infos) != 2 {
		t.Fatalf("list: live=%q sessions=%d, want live=%q sessions=2", live, len(infos), newID)
	}
	// /status names the live session.
	if st := a.StatusLineForChat(chatID); !strings.Contains(st, "Session: "+newID) {
		t.Fatalf("/status lacks the live session id:\n%s", st)
	}
	// Turns after rotation land in the NEW transcript only.
	if _, err := agent.Run(ctx, "second session turn"); err != nil {
		t.Fatalf("agent Run after rotation: %v", err)
	}
	cs, _ := a.chatSessionFor(chatID)
	if cs.transcript == nil || cs.transcript.Count() != 2 {
		t.Fatalf("new transcript count=%v, want 2", cs.transcript)
	}
	// The old transcript is untouched by the new session's turns.
	oldTrMsgs := 0
	if data, err := os.ReadFile(oldPath); err == nil {
		oldTrMsgs = strings.Count(string(data), "\n")
	}
	if oldTrMsgs != oldCount {
		t.Fatalf("old transcript grew after rotation: %d lines, want %d", oldTrMsgs, oldCount)
	}
}

// TestResumeSessionForChatRestoresHistory verifies /resume restores
// a named session's turns into working history, moves the live
// pointer, and re-points the agent's transcript sink at it —
// facts and typed memories are shared and untouched.
func TestResumeSessionForChatRestoresHistory(t *testing.T) {
	dir := t.TempDir()
	a := testAppOnDir(t, dir)
	chatID := int64(9)
	ctx := context.Background()

	agent, err := a.AgentForChat(chatID)
	if err != nil {
		t.Fatalf("AgentForChat: %v", err)
	}
	if _, err := agent.Run(ctx, "session A turn"); err != nil {
		t.Fatalf("Run A: %v", err)
	}
	idA, err := a.CurrentSessionIDForChat(chatID)
	if err != nil || idA == "" {
		t.Fatalf("CurrentSessionIDForChat A: %q %v", idA, err)
	}
	if err := a.NewSessionForChat(chatID); err != nil {
		t.Fatalf("NewSessionForChat: %v", err)
	}
	if _, err := agent.Run(ctx, "session B turn"); err != nil {
		t.Fatalf("Run B: %v", err)
	}
	if got := chatMessages(t, a, chatID); !containsText(got, "session B turn") {
		t.Fatalf("session B history missing its turn: %v", contents(got))
	}

	// Resume A by full id.
	resolved, n, err := a.ResumeSessionForChat(chatID, idA)
	if err != nil {
		t.Fatalf("ResumeSessionForChat: %v", err)
	}
	if resolved != idA {
		t.Fatalf("resumed id=%q, want %q", resolved, idA)
	}
	if n != 2 {
		t.Fatalf("resumed message count=%d, want 2 (one user + one assistant turn)", n)
	}
	got := chatMessages(t, a, chatID)
	if !containsText(got, "session A turn") || containsText(got, "session B turn") {
		t.Fatalf("history after resume is not session A's: %v", contents(got))
	}
	if live, _ := a.CurrentSessionIDForChat(chatID); live != idA {
		t.Fatalf("live session after resume=%q, want %q", live, idA)
	}
	// A turn after resume is recorded into A's transcript.
	cs, _ := a.chatSessionFor(chatID)
	before := cs.transcript.Count()
	if _, err := agent.Run(ctx, "back in session A"); err != nil {
		t.Fatalf("Run after resume: %v", err)
	}
	if cs.transcript.Count() != before+2 {
		t.Fatalf("resumed transcript count=%d, want %d", cs.transcript.Count(), before+2)
	}
	// Resuming the live session is a no-op reporting its count.
	if resolved, n, err := a.ResumeSessionForChat(chatID, idA); err != nil || resolved != idA || n != 4 {
		t.Fatalf("resume live: id=%q n=%d err=%v, want %q/4/nil", resolved, n, err, idA)
	}
	// An unknown id is an error, and history is untouched.
	if _, _, err := a.ResumeSessionForChat(chatID, "20990101-000000-deadbeef"); err == nil {
		t.Fatalf("resuming an unknown session should fail")
	}
	if got := chatMessages(t, a, chatID); len(got) != 4 {
		t.Fatalf("history changed after failed resume: %d messages", len(got))
	}
}

// TestGlobalTranscriptSurvivesAppRebuild simulates a daemon
// restart: a second App over the same data dir must pick up the
// same live session and the same transcript — no duplication, no
// loss — and /new + /resume must work across the restart.
func TestGlobalTranscriptSurvivesAppRebuild(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	app1 := testAppOnDir(t, dir)
	if err := app1.Memory.AddMessage(model.Message{Role: "user", Content: "legacy turn one"}); err != nil {
		t.Fatalf("seed AddMessage: %v", err)
	}
	if err := app1.Memory.AddMessage(model.Message{Role: "assistant", Content: "legacy reply"}); err != nil {
		t.Fatalf("seed AddMessage: %v", err)
	}
	// First wire migrates the pre-transcript history.
	app1.wireGlobalTranscript()
	id1 := app1.CurrentSessionID()
	if id1 == "" {
		t.Fatalf("global session id empty after wire")
	}
	if _, err := app1.Agent.Run(ctx, "hello after migration"); err != nil {
		t.Fatalf("app1 Run: %v", err)
	}
	if app1.globalTranscript.Count() != 4 {
		t.Fatalf("app1 transcript count=%d, want 4 (2 migrated + 2 from the turn)", app1.globalTranscript.Count())
	}

	// "Restart": a fresh App over the same data dir.
	app2 := testAppOnDir(t, dir)
	app2.wireGlobalTranscript()
	if app2.CurrentSessionID() != id1 {
		t.Fatalf("restarted app live session=%q, want %q", app2.CurrentSessionID(), id1)
	}
	msgs, err := app2.globalTranscript.Messages()
	if err != nil {
		t.Fatalf("restarted transcript Messages: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("restarted transcript messages=%d, want 4 (migration must not duplicate)", len(msgs))
	}
	if !containsText(msgs, "legacy turn one") || !containsText(msgs, "hello after migration") {
		t.Fatalf("restarted transcript lost turns: %v", contents(msgs))
	}
	infos, live, err := app2.ListSessions()
	if err != nil || live != id1 || len(infos) != 1 {
		t.Fatalf("restarted ListSessions: live=%q n=%d err=%v, want %q/1/nil", live, len(infos), err, id1)
	}

	// /new + /resume across the restart.
	if err := app2.NewSession(); err != nil {
		t.Fatalf("app2 NewSession: %v", err)
	}
	if app2.CurrentSessionID() == id1 {
		t.Fatalf("NewSession did not rotate the global session")
	}
	resolved, n, err := app2.ResumeSession(id1)
	if err != nil {
		t.Fatalf("app2 ResumeSession: %v", err)
	}
	if resolved != id1 || n != 4 {
		t.Fatalf("resumed global: id=%q n=%d, want %q/4", resolved, n, id1)
	}
	restored, err := app2.Memory.RecentMessages(0)
	if err != nil || len(restored) != 4 || !containsText(restored, "legacy turn one") {
		t.Fatalf("global history after resume: %d messages, want 4 with the legacy turn", len(restored))
	}
}

// TestManualCompressResetsBreakerAndRecordsSummary is the app-level
// proof for audit P0 #3's circuit breaker: three failed
// auto-compactions trip the breaker, and a successful manual
// /compress clears it again — and the manual summary lands in the
// live session transcript as well as the conversation-summary fact.
func TestManualCompressResetsBreakerAndRecordsSummary(t *testing.T) {
	dir := t.TempDir()
	a := testAppOnDir(t, dir)
	ctx := context.Background()
	a.wireGlobalTranscript()

	// 170 short messages: count pressure 170/200 = 85%, over the
	// 80% auto-compact trigger, so every turn tries to compact.
	for i := 0; i < 170; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := a.Memory.AddMessage(model.Message{Role: role, Content: "filler turn message"}); err != nil {
			t.Fatalf("seed AddMessage %d: %v", i, err)
		}
	}
	a.Agent.SetCompactionSummariser(func(ctx context.Context, old []model.Message) (string, error) {
		return "", errors.New("boom: summariser unavailable")
	})
	for i := 0; i < 3; i++ {
		if _, err := a.Agent.Run(ctx, "trigger compaction"); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	st := a.Agent.CompactionStatus()
	if !st.Tripped || st.ConsecutiveFailures != 3 {
		t.Fatalf("breaker after 3 failures: tripped=%v failures=%d, want true/3", st.Tripped, st.ConsecutiveFailures)
	}
	// /status surfaces the tripped breaker for the CLI/global app
	// through the chat path's twin (CompactionStatusForChat is
	// covered by the chat tests); here the agent-level state is
	// the assertion, plus the manual-compress reset below.

	before, after, summaryLen, err := a.Compress(ctx, 10)
	if err != nil {
		t.Fatalf("Compress after trip: %v", err)
	}
	if before <= 170 || after != 10 || summaryLen == 0 {
		t.Fatalf("Compress: before=%d after=%d summaryLen=%d, want >170/10/>0", before, after, summaryLen)
	}
	if st := a.Agent.CompactionStatus(); st.Tripped || st.ConsecutiveFailures != 0 {
		t.Fatalf("breaker after manual /compress: tripped=%v failures=%d, want false/0", st.Tripped, st.ConsecutiveFailures)
	}
	summaries, err := a.globalTranscript.Summaries()
	if err != nil {
		t.Fatalf("transcript Summaries: %v", err)
	}
	if len(summaries) != 1 || !strings.Contains(summaries[0], "ok") {
		t.Fatalf("transcript summaries=%v, want the one manual-compress summary", summaries)
	}
	// The manual summary also reached the fact, as before.
	fact, found, err := a.Memory.GetFact("conversation-summary")
	if err != nil || !found || !strings.Contains(fact.Value, "ok") {
		t.Fatalf("summary fact: found=%v value=%q err=%v", found, fact.Value, err)
	}
}
