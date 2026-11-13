package messaging

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

// --- Callback delivery under a full turn semaphore (rc11 W7) --------
//
// Live incident (rc10, 2026-11-13 18:30 SGT): Irfan tapped Approve on
// approval card 253 (request 3908aaf739a79854) in-window, and the gate
// timed out with ZERO tap lines in the daemon log — twice in a row
// (card 245 expired the same way). The prompt and the expiry ARE
// logged, so the card went out and the timer ran; the tap simply never
// reached the callback handler.
//
// The delivery path has one silent stall point: PollStream dispatches
// a callback query only after acquiring a slot on the SAME semaphore
// that bounds turn handlers (maxPollHandlers = 8), and it acquires
// that slot in the poll loop — before `go handleCallback`. A turn
// handler holds its slot for the whole turn, including a 90s approval
// wait and multi-minute model runs, so with all 8 slots held the poll
// loop blocks mid-batch and the tap is never dispatched — no log line,
// no error, no drop: exactly the live signature. (A second poller
// confirming the update server-side would also produce zero lines;
// that case is outside this process and is covered by the new
// per-update poll logging instead — see logReceivedUpdate.)
//
// The regression test below fills all 8 turn slots with blocked turns
// and asserts a delivered callback still reaches its handler promptly.
// It FAILS on the old code (the poll loop stalls on the turn
// semaphore) and PASSES once callbacks use their own semaphore.

// queueFake is a Telegram-faithful fake Bot API for delivery tests:
// getUpdates long-polls (blocks until an update with update_id >=
// offset exists or the requested timeout elapses) and filters ONLY by
// the offset — it never consumes updates on return, exactly like the
// real Bot API, where an update is confirmed only by a later
// getUpdates call with a higher offset. The wait also aborts when the
// client goes away, so test teardown is fast.
type queueFake struct {
	mu       sync.Mutex
	queue    []Update
	answered []string
	sent     []queueSent // every sendMessage call, in order
	edits    []queueEdit // every editMessageText call, in order
	nextID   int64
}

// queueSent is one sendMessage call the fake recorded.
type queueSent struct {
	ChatID   int64  `json:"chat_id"`
	Text     string `json:"text"`
	Keyboard string // raw reply_markup JSON, "" when no keyboard
	MsgID    int64
}

// queueEdit is one editMessageText call the fake recorded.
type queueEdit struct {
	ChatID  int64  `json:"chat_id"`
	MsgID   int64  `json:"message_id"`
	Text    string `json:"text"`
	HasKeys bool   // the edit carried a reply_markup (empty keyboard)
}

func newQueueFake() *queueFake { return &queueFake{} }

func (f *queueFake) enqueue(u Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, u)
}

func (f *queueFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	if strings.HasSuffix(r.URL.Path, "/getUpdates") {
		q := r.URL.Query()
		offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		timeout, _ := strconv.Atoi(q.Get("timeout"))
		if timeout <= 0 {
			timeout = 30
		}
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
		var msg queueSent
		_ = json.Unmarshal(body, &msg)
		var raw struct {
			ReplyMarkup json.RawMessage `json:"reply_markup"`
		}
		_ = json.Unmarshal(body, &raw)
		msg.Keyboard = string(raw.ReplyMarkup)
		f.nextID++
		msg.MsgID = f.nextID
		f.sent = append(f.sent, msg)
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, f.nextID)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		var e queueEdit
		_ = json.Unmarshal(body, &e)
		var raw struct {
			ReplyMarkup json.RawMessage `json:"reply_markup"`
		}
		_ = json.Unmarshal(body, &raw)
		e.HasKeys = len(raw.ReplyMarkup) > 0
		f.edits = append(f.edits, e)
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

// TestCallbackDeliveredWhileTurnSlotsFull: with all 8 turn-handler
// slots held by blocked turns, a callback-query update delivered by
// getUpdates must still reach the installed callback handler promptly
// (well inside any approval window). On the old code the poll loop
// blocks acquiring a turn slot for the callback and the handler is
// never invoked — the live rc10 signature (tap in-window, zero tap
// lines, gate times out).
func TestCallbackDeliveredWhileTurnSlotsFull(t *testing.T) {
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77, 78, 79, 80, 81, 82, 83, 84))
	if err != nil {
		t.Fatal(err)
	}

	// Eight turns that block forever (until the test releases them),
	// one per chat so none waits on another chat's lock: each holds
	// one turn-semaphore slot for its whole duration.
	release := make(chan struct{})
	var entered atomic.Int32
	turnHandler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		entered.Add(1)
		<-release
		return "done", nil, nil
	}
	cbDone := make(chan TGCallbackQuery, 1)
	g.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		cbDone <- q
		return g.AnswerCallbackQuery(ctx, q.ID, "ok")
	})

	cancel, errCh := runPollStream(t, g, turnHandler)
	defer func() {
		close(release)
		stopPollStream(t, cancel, errCh)
	}()

	// Fill all 8 turn slots: 8 messages, one per chat.
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

	// The tap: a callback query for a pending approval, delivered in
	// a later poll cycle while every turn slot is held.
	fake.enqueue(Update{UpdateID: 9, CallbackQuery: &TGCallbackQuery{
		ID:      "cb-tap-1",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 9, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:0123456789abcdef",
	}})
	select {
	case q := <-cbDone:
		if q.Data != "perm:approve:0123456789abcdef" {
			t.Errorf("callback data = %q, want the approval payload", q.Data)
		}
		if q.ID != "cb-tap-1" {
			t.Errorf("callback id = %q, want cb-tap-1", q.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback query never reached the handler while all 8 turn slots were held (poll loop stalled on the turn semaphore)")
	}
	waitFor(t, "callback answer", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.answered) == 1 && fake.answered[0] == "cb-tap-1"
	})
}
