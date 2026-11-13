// In-batch callback ordering + crash-window offset tests (F-A7-P2-3).
//
// The rc11 W7 fix gave callbacks their own semaphore, so a tap in a
// FRESH batch is dispatched even while all turn slots are held. What
// remained: the poll loop iterated a batch sequentially, so a message
// update earlier in the SAME batch still blocked the loop on the
// turn semaphore before a callback later in that batch was reached
// — a tap could wait behind a message dispatch for up to the 90s
// approval window. PollStream now dispatches every callback in a
// batch before any message in it.
//
// The offset used to advance in GetUpdates before dispatch, so a
// daemon dying mid-batch confirmed fetched-but-undispatched updates
// server-side and they were lost. The offset now advances only past
// the longest contiguous run of decided updates, so the undispatched
// suffix is re-delivered instead.
//
// Both tests FAIL on rc11 code: the ordering test times out (the
// tap never reaches the handler while the loop waits on a turn
// slot), and the offset test sees the offset jump past the
// undispatched message at fetch time.
package messaging

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// fillTurnSlots starts PollStream with turns that block until
// release is closed and enqueues one message per chat, waiting
// until all 8 turn slots are held. It returns the cancel func, the
// error channel, the release channel (close it once via
// releaseAll), and the entered counter.
func fillTurnSlots(t *testing.T, fake *queueFake, g *TelegramGateway) (context.CancelFunc, chan error, chan struct{}, func(), *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	var entered atomic.Int32
	turnHandler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		entered.Add(1)
		<-release
		return "done", nil, nil
	}
	cancel, errCh := runPollStream(t, g, turnHandler)
	for i := int64(0); i < 8; i++ {
		chatID := 77 + i
		fake.enqueue(Update{UpdateID: i + 1, Message: &TGMessage{
			MessageID: i + 1, Chat: TGChat{ID: chatID},
			Text: fmt.Sprintf("msg %d", i+1), From: &TGUser{ID: chatID},
		}})
	}
	waitFor(t, "all 8 turn slots held", func() bool {
		return entered.Load() == 8
	})
	return cancel, errCh, release, releaseAll, &entered
}

// TestCallbackDispatchedBeforeMessageInSameBatchWhileTurnSlotsFull:
// with all 8 turn slots held, a batch whose FIRST update is a
// message and whose SECOND is a callback query must still deliver
// the callback promptly — the tap must not wait behind the
// message's turn-slot acquisition. The message itself must not be
// dropped to make room: once a slot frees it runs.
func TestCallbackDispatchedBeforeMessageInSameBatchWhileTurnSlotsFull(t *testing.T) {
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77, 78, 79, 80, 81, 82, 83, 84))
	if err != nil {
		t.Fatal(err)
	}
	cbDone := make(chan TGCallbackQuery, 1)
	g.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		cbDone <- q
		return g.AnswerCallbackQuery(ctx, q.ID, "ok")
	})

	cancel, errCh, _, releaseAll, entered := fillTurnSlots(t, fake, g)
	defer func() {
		releaseAll()
		stopPollStream(t, cancel, errCh)
	}()

	// One batch: a message FIRST, then the tap. On rc11 the loop
	// blocks acquiring a turn slot for the message and the tap is
	// never reached until a slot frees.
	fake.enqueue(Update{UpdateID: 9, Message: &TGMessage{
		MessageID: 9, Chat: TGChat{ID: 77},
		Text: "late message", From: &TGUser{ID: 77},
	}})
	fake.enqueue(Update{UpdateID: 10, CallbackQuery: &TGCallbackQuery{
		ID:      "cb-tap-2",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 10, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:0123456789abcdef",
	}})
	select {
	case q := <-cbDone:
		if q.ID != "cb-tap-2" {
			t.Errorf("callback id = %q, want cb-tap-2", q.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback later in the same batch never reached the handler while all 8 turn slots were held behind the earlier message (F-A7-P2-3)")
	}
	waitFor(t, "callback answered", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.answered) == 1 && fake.answered[0] == "cb-tap-2"
	})

	// The message was not dropped: once a slot frees, it runs.
	releaseAll()
	waitFor(t, "blocked message dispatched after release", func() bool {
		return entered.Load() == 9
	})
}

// TestOffsetNotAdvancedPastUndispatchedMessage: with all 8 turn
// slots held, a batch [message 9, callback 10] must advance the
// offset past the dispatched callback only in the sense that the
// callback is decided — the offset watermark itself must stay
// behind the undispatched message 9, so a crash now re-delivers
// the suffix instead of confirming and losing it. Once the
// message is dispatched the watermark passes the whole batch.
func TestOffsetNotAdvancedPastUndispatchedMessage(t *testing.T) {
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77, 78, 79, 80, 81, 82, 83, 84))
	if err != nil {
		t.Fatal(err)
	}
	cbDone := make(chan TGCallbackQuery, 1)
	g.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		cbDone <- q
		return g.AnswerCallbackQuery(ctx, q.ID, "ok")
	})

	cancel, errCh, _, releaseAll, entered := fillTurnSlots(t, fake, g)
	defer func() {
		releaseAll()
		stopPollStream(t, cancel, errCh)
	}()

	if got := g.Offset(); got != 9 {
		t.Fatalf("offset after filling turn slots = %d, want 9 (past the 8 dispatched messages)", got)
	}
	fake.enqueue(Update{UpdateID: 9, Message: &TGMessage{
		MessageID: 9, Chat: TGChat{ID: 77},
		Text: "late message", From: &TGUser{ID: 77},
	}})
	fake.enqueue(Update{UpdateID: 10, CallbackQuery: &TGCallbackQuery{
		ID:      "cb-tap-3",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 10, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:0123456789abcdef",
	}})
	select {
	case <-cbDone:
	case <-time.After(5 * time.Second):
		t.Fatal("callback in the batch never reached the handler")
	}
	// The callback is dispatched, but message 9 is still waiting
	// on a turn slot — the watermark must not pass it. On rc11 the
	// offset jumped to 11 at fetch time, confirming message 9
	// before it was ever dispatched.
	if got := g.Offset(); got != 9 {
		t.Fatalf("offset while message 9 is undispatched = %d, want 9 (the watermark must stay behind the undispatched message)", got)
	}

	releaseAll()
	waitFor(t, "blocked message dispatched after release", func() bool {
		return entered.Load() == 9
	})
	waitFor(t, "offset past the whole batch", func() bool {
		return g.Offset() == 11
	})
}
