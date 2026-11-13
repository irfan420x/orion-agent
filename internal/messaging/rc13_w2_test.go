package messaging

// F-A8-2 (P1, rc13 W2) — PollStream crash window.
//
// rc12 narrowed the window from fetch-time to dispatch-time, but the
// offset was still confirmed the moment a handler goroutine was
// SPAWNED (`go handle(upd)` + `markDecided` immediately after), not
// when it completed. The poll loop then re-issued getUpdates with
// the advanced offset, confirming the in-flight turn server-side.
// A crash/restart any time during a minutes-long turn lost the
// update permanently: never re-delivered, no reply. Four silent VM
// reboots happened on 2026-11-13, so this was live risk.
//
// The fix: the offset advances only when the handler COMPLETES
// (markDecided runs as a defer inside handle/handleCallback, after
// the reply is sent / the query is answered). While a turn is
// in-flight the offset stays behind it, so a later getUpdates
// re-fetches it; the poll loop deduplicates in-flight update IDs
// instead of dispatching the turn twice.
//
// The tests below are written against the W4 audit probe shape: a
// fake Bot API that records every requested getUpdates offset, one
// message, handler blocked mid-turn — and assert no getUpdates was
// sent with an offset past the in-flight update's ID while its
// handler was still running. They FAIL on unmodified rc12 code
// (offset jumps to 2 at spawn) and PASS with the fix.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// w2OffsetFake is a Telegram-faithful fake Bot API for the F-A8-2
// tests: like queueFake it filters getUpdates ONLY by the requested
// offset and never consumes updates on return (an update is
// confirmed only by a later getUpdates with a higher offset), and
// it additionally records every offset getUpdates was asked for,
// in order. The getUpdates wait aborts when the client goes away,
// so test teardown is fast.
type w2OffsetFake struct {
	mu       sync.Mutex
	queue    []Update
	offsets  []int64 // every offset a getUpdates call requested
	answered []string
	sent     []string // sendMessage texts, in order
	nextID   int64
}

func newW2OffsetFake() *w2OffsetFake { return &w2OffsetFake{} }

func (f *w2OffsetFake) enqueue(u Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, u)
}

func (f *w2OffsetFake) offsetsSnapshot() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.offsets...)
}

func (f *w2OffsetFake) sentSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *w2OffsetFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	if strings.HasSuffix(r.URL.Path, "/getUpdates") {
		q := r.URL.Query()
		offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		timeout, _ := strconv.Atoi(q.Get("timeout"))
		if timeout <= 0 {
			timeout = 30
		}
		f.mu.Lock()
		f.offsets = append(f.offsets, offset)
		f.mu.Unlock()
		deadline := time.Now().Add(time.Duration(timeout) * time.Second)
		var out []Update
		for {
			f.mu.Lock()
			out = nil
			for _, u := range f.queue {
				if u.UpdateID >= offset {
					out = append(out, u)
				}
			}
			f.mu.Unlock()
			if len(out) > 0 || time.Now().After(deadline) {
				break
			}
			select {
			case <-r.Context().Done():
				out = []Update{}
				data, _ := json.Marshal(map[string]any{"ok": true, "result": out})
				w.Write(data)
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		if out == nil {
			out = []Update{}
		}
		data, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		w.Write(data)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		var msg struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &msg)
		f.nextID++
		f.sent = append(f.sent, msg.Text)
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, f.nextID)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
		var req struct {
			CallbackQueryID string `json:"callback_query_id"`
		}
		_ = json.Unmarshal(body, &req)
		f.answered = append(f.answered, req.CallbackQueryID)
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"orion_test_bot","first_name":"Orion"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"description":"not found"}`)
	}
}

// TestOffsetNotAdvancedPastInFlightTurn is the F-A8-2 regression
// test, in the Audit 8 W4 probe shape: one message (update 1),
// handler blocked mid-turn. While the turn is in-flight, no
// getUpdates may be sent with an offset past update 1 — on rc12
// the offset jumped to 2 the moment the handler was spawned, so
// the very next getUpdates confirmed the turn server-side and a
// crash mid-turn would lose it forever. The turn must also be
// dispatched exactly once despite being re-fetched on every poll
// (in-flight dedup), and the offset must advance past it once the
// handler completes.
func TestOffsetNotAdvancedPastInFlightTurn(t *testing.T) {
	fake := newW2OffsetFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77, 78))
	if err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	var entered atomic.Int32
	turnHandler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		entered.Add(1)
		<-release
		return "done", nil, nil
	}

	fake.enqueue(Update{UpdateID: 1, Message: &TGMessage{
		MessageID: 1, Chat: TGChat{ID: 77}, Text: "slow turn", From: &TGUser{ID: 77},
	}})

	cancel, errCh := runPollStream(t, g, turnHandler)
	defer func() {
		releaseAll()
		stopPollStream(t, cancel, errCh)
	}()

	waitFor(t, "turn handler entered", func() bool { return entered.Load() == 1 })

	// Let the poll loop re-issue getUpdates several times while the
	// turn is still in-flight. On rc12 the offset is already 2 by
	// now, so the next getUpdates (offset 2) is the smoking gun.
	// Give the loop enough cycles to prove the point either way.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	offsets := fake.offsetsSnapshot()
	if len(offsets) < 2 {
		t.Fatalf("getUpdates offsets = %v, want at least 2 polls while the turn is in-flight (the loop must keep polling, not stall)", offsets)
	}
	for _, off := range offsets {
		if off >= 2 {
			t.Fatalf("getUpdates requested offset %d while the turn for update 1 was still in-flight (offsets seen: %v) — the in-flight turn was confirmed server-side and a crash now would lose it (F-A8-2)", off, offsets)
		}
	}
	if got := g.Offset(); got != 1 {
		t.Fatalf("gateway offset while turn 1 is in-flight = %d, want 1 (the watermark must stay behind the in-flight turn)", got)
	}
	// The re-fetched update must not have been dispatched a second
	// time: in-flight dedup, not double execution.
	if got := entered.Load(); got != 1 {
		t.Fatalf("turn handler entered %d times while update 1 was re-fetched in-flight, want exactly 1 (re-delivery must not double-execute a running turn)", got)
	}

	// Release the turn: it completes, replies, and only then may
	// the offset advance past it.
	releaseAll()
	waitFor(t, "reply after turn completes", func() bool {
		for _, s := range fake.sentSnapshot() {
			if s == "done" {
				return true
			}
		}
		return false
	})
	waitFor(t, "offset past the completed turn", func() bool { return g.Offset() == 2 })
	if got := entered.Load(); got != 1 {
		t.Fatalf("turn handler entered %d times in total, want exactly 1", got)
	}
}

// TestOffsetNotAdvancedPastInFlightCallback is the callback half of
// F-A8-2: a callback query whose handler is blocked mid-flight
// must not be confirmed either. The two-pass callbacks-first
// dispatch and the dedicated cbSem are preserved: the tap is
// dispatched promptly even while turn slots are held, and it is
// answered exactly once.
func TestOffsetNotAdvancedPastInFlightCallback(t *testing.T) {
	fake := newW2OffsetFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	var cbEntered atomic.Int32
	g.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		cbEntered.Add(1)
		<-release
		return g.AnswerCallbackQuery(ctx, q.ID, "ok")
	})
	// Turns return immediately; the test is about the callback.
	turnHandler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "done", nil, nil
	}

	fake.enqueue(Update{UpdateID: 1, CallbackQuery: &TGCallbackQuery{
		ID:      "cb-w2-1",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:0123456789abcdef",
	}})

	cancel, errCh := runPollStream(t, g, turnHandler)
	defer func() {
		releaseAll()
		stopPollStream(t, cancel, errCh)
	}()

	waitFor(t, "callback handler entered", func() bool { return cbEntered.Load() == 1 })

	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	for _, off := range fake.offsetsSnapshot() {
		if off >= 2 {
			t.Fatalf("getUpdates requested offset %d while the callback for update 1 was still in-flight — the in-flight tap was confirmed server-side (F-A8-2)", off)
		}
	}
	if got := g.Offset(); got != 1 {
		t.Fatalf("gateway offset while callback 1 is in-flight = %d, want 1", got)
	}
	if got := cbEntered.Load(); got != 1 {
		t.Fatalf("callback handler entered %d times while update 1 was re-fetched in-flight, want exactly 1", got)
	}

	releaseAll()
	waitFor(t, "callback answered after handler completes", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.answered) == 1 && fake.answered[0] == "cb-w2-1"
	})
	waitFor(t, "offset past the completed callback", func() bool { return g.Offset() == 2 })
	if got := cbEntered.Load(); got != 1 {
		t.Fatalf("callback handler entered %d times in total, want exactly 1", got)
	}
}

// TestOffsetWatermarkWaitsForSlowestTurn: updates 1 and 2 in
// different chats; the turn for 2 finishes first, the turn for 1
// stays blocked. The offset is a single watermark, so it must not
// pass 1 even though 2 is already complete — otherwise a crash
// would confirm (and lose) turn 1 while only turn 2 was done.
// Once turn 1 completes, the watermark jumps past both.
func TestOffsetWatermarkWaitsForSlowestTurn(t *testing.T) {
	fake := newW2OffsetFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77, 78))
	if err != nil {
		t.Fatal(err)
	}

	release1 := make(chan struct{})
	var release1Once sync.Once
	release1All := func() { release1Once.Do(func() { close(release1) }) }
	var entered1, entered2 atomic.Int32
	turnHandler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		if chatID == 77 {
			entered1.Add(1)
			<-release1
			return "one", nil, nil
		}
		entered2.Add(1)
		return "two", nil, nil
	}

	fake.enqueue(Update{UpdateID: 1, Message: &TGMessage{
		MessageID: 1, Chat: TGChat{ID: 77}, Text: "slow", From: &TGUser{ID: 77},
	}})
	fake.enqueue(Update{UpdateID: 2, Message: &TGMessage{
		MessageID: 2, Chat: TGChat{ID: 78}, Text: "fast", From: &TGUser{ID: 78},
	}})

	cancel, errCh := runPollStream(t, g, turnHandler)
	defer func() {
		release1All()
		stopPollStream(t, cancel, errCh)
	}()

	waitFor(t, "both turn handlers entered", func() bool {
		return entered1.Load() == 1 && entered2.Load() == 1
	})
	// Turn 2 has returned; turn 1 is still blocked. Let the loop
	// poll a few more times, then check the watermark.
	time.Sleep(800 * time.Millisecond)
	if got := g.Offset(); got != 1 {
		t.Fatalf("gateway offset while turn 1 is in-flight (turn 2 already done) = %d, want 1 (the watermark must wait for the slowest in-flight turn)", got)
	}
	for _, off := range fake.offsetsSnapshot() {
		if off >= 2 {
			t.Fatalf("getUpdates requested offset %d while turn 1 was still in-flight (turn 2 done) — turn 1 was confirmed server-side (F-A8-2)", off)
		}
	}
	if got := entered1.Load(); got != 1 {
		t.Fatalf("slow turn handler entered %d times, want exactly 1", got)
	}

	release1All()
	waitFor(t, "offset past both turns", func() bool { return g.Offset() == 3 })
}

// TestOffsetNoDoubleExecutionAfterRelease covers the stale-batch
// hole found while fixing F-A8-2: a getUpdates issued with an older
// offset can return AFTER the watermark has advanced past some of
// its updates (a long poll lasts up to 30s). If the poll loop had
// already forgotten those updates were decided, the stale batch
// re-dispatched their turns a second time. Eight blocked turns,
// a late message and a tap, then release: every turn must run
// exactly once, and the offset must end past the whole batch.
func TestOffsetNoDoubleExecutionAfterRelease(t *testing.T) {
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

	fake.enqueue(Update{UpdateID: 9, Message: &TGMessage{
		MessageID: 9, Chat: TGChat{ID: 77},
		Text: "late message", From: &TGUser{ID: 77},
	}})
	fake.enqueue(Update{UpdateID: 10, CallbackQuery: &TGCallbackQuery{
		ID:      "cb-tap-w2",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 10, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:0123456789abcdef",
	}})
	select {
	case <-cbDone:
	case <-time.After(5 * time.Second):
		t.Fatal("callback in the batch never reached the handler")
	}

	releaseAll()
	waitFor(t, "blocked message dispatched after release", func() bool {
		return entered.Load() == 9
	})
	waitFor(t, "offset past the whole batch", func() bool {
		return g.Offset() == 11
	})
	// Let several poll cycles run: a stale batch returning
	// already-decided updates must not re-run any turn.
	time.Sleep(1500 * time.Millisecond)
	if got := entered.Load(); got != 9 {
		t.Fatalf("turn handler entered %d times in total, want exactly 9 (a stale getUpdates batch re-dispatched already-finished turns)", got)
	}
}
