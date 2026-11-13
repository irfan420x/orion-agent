package messaging

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// TestPollStreamPassesUpdateIDToHandler (F-A9-1) proves the turn
// handler receives its Telegram update id through the context:
// the agent loop reads it from there to replay recorded tool
// results from the durable tool journal instead of re-executing
// them on a re-delivered turn.
func TestPollStreamPassesUpdateIDToHandler(t *testing.T) {
	fake := &streamFake{updates: []Update{{
		UpdateID: 100,
		Message: &TGMessage{
			MessageID: 1, Chat: TGChat{ID: 77},
			Text: "hi", From: &TGUser{ID: 77},
		},
	}}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	var gotID int64
	var calls int32
	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		gotID = core.UpdateIDFromContext(ctx)
		atomic.AddInt32(&calls, 1)
		return "ok", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)
	waitFor(t, "handler ran", func() bool { return atomic.LoadInt32(&calls) == 1 })
	if gotID != 100 {
		t.Errorf("handler saw update id %d, want 100", gotID)
	}
	waitFor(t, "offset past the update", func() bool { return g.Offset() == 101 })
}

// TestPollStreamPanickedTurnStopsTyping (F-A9-P2-7) proves a
// handler panic does not leak the typing goroutine: after the
// panic is recovered and the apology sent, no further
// sendChatAction calls arrive. The old code closed typingDone
// on the normal return path only, so a panicked turn sent one
// "typing" POST every 4s until daemon restart (2 extra calls
// in 9s, measured by Audit 9). The observation window is 9s —
// just over two 4s ticker intervals — so a leak cannot hide.
func TestPollStreamPanickedTurnStopsTyping(t *testing.T) {
	fake := &streamFake{updates: []Update{{
		UpdateID: 100,
		Message: &TGMessage{
			MessageID: 1, Chat: TGChat{ID: 77},
			Text: "boom", From: &TGUser{ID: 77},
		},
	}}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	var calls int32
	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		atomic.AddInt32(&calls, 1)
		panic("test panic: boom")
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)
	// The panic is recovered, the apology is sent, and the
	// update is confirmed — the turn is over.
	waitFor(t, "apology sent", func() bool {
		sent, _, _ := fake.snapshot()
		for _, s := range sent {
			if strings.Contains(s.Text, "Sorry, I hit an error") {
				return true
			}
		}
		return false
	})
	waitFor(t, "offset past the update", func() bool { return g.Offset() == 101 })
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
	_, _, actionsAfter := fake.snapshot()
	// Two ticker intervals (4s each) plus margin: a leaked
	// goroutine would post at least twice more.
	time.Sleep(9 * time.Second)
	_, _, actionsNow := fake.snapshot()
	if len(actionsNow) != len(actionsAfter) {
		t.Errorf("sendChatAction calls grew from %d to %d after the panicked turn ended (leaked typing goroutine)", len(actionsAfter), len(actionsNow))
	}
	// Polling itself must still be alive: a second update is
	// handled after the panic.
	fake.mu.Lock()
	fake.updates = []Update{{
		UpdateID: 101,
		Message: &TGMessage{
			MessageID: 2, Chat: TGChat{ID: 77},
			Text: "after", From: &TGUser{ID: 77},
		},
	}}
	fake.updateCalls = 0
	fake.mu.Unlock()
	waitFor(t, "second update handled after the panic", func() bool { return atomic.LoadInt32(&calls) == 2 })
}
