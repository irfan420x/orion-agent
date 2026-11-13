package messaging

// W6 (rc12) — end-to-end approval-callback reproduction, live shape.
//
// Live incident (rc11 daemon, PID 45059): card 257, request
// 6bb3cd6b94890aec, prompted ~20:28:30 SGT, user tapped in-window,
// expired 20:30:03 with ZERO callback_query updates and ZERO tap
// lines — while message update 181487684 WAS received and logged.
// Update-id arithmetic proves Telegram never created a callback_query
// for the tap: the next update after the command (181487684) is
// 181487685 and it is a MESSAGE, so no callback was ever assigned an
// update_id between them. These tests drive the REAL gate +
// ApprovalPrompter + callback handler + PollStream against a fake Bot
// API that delivers a REAL-shaped callback_query update (full
// production JSON, with the extra fields Telegram sends) carrying the
// EXACT callback_data the sent card carried, while a turn is blocked
// on gate.Check — the live shape. They pin the in-process half of
// the round-trip: card -> tap -> resolve -> turn proceeds.
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/permissions"
)

const (
	w6ChatID   int64 = 7429947930
	w6OwnerID  int64 = 7429947930
	w6CmdUpd   int64 = 181487684
	w6CbUpd    int64 = 181487685
	w6SecondCb int64 = 181487686
)

// w6FakeBot is a scripted fake Telegram Bot API. It captures every
// sendMessage body (so the test asserts on the card that was ACTUALLY
// sent), serves getUpdates from a script, and records
// answerCallbackQuery / editMessageText calls.
type w6FakeBot struct {
	mu       sync.Mutex
	sent     []w6CapturedSend // sendMessage bodies, in order
	answers  []map[string]any // answerCallbackQuery bodies
	edits    []map[string]any // editMessageText bodies
	actions  int              // sendChatAction calls
	nextMsg  int64            // next message_id to hand out
	getCalls int

	// Script for the callback leg. When release is closed, the next
	// getUpdates returns the tap (once per entry in taps).
	taps    []w6Tap
	tapIdx  int
	release chan struct{}

	// If non-nil, getUpdates returns this batch on its first call
	// instead of the scripted command message (same-batch test).
	firstBatch []byte
}

type w6CapturedSend struct {
	ChatID int64
	Text   string
	Raw    map[string]any
}

type w6Tap struct {
	UpdateID int64
	QueryID  string
	Data     string // empty = use the Approve button's data from the sent card
}

func newW6FakeBot() *w6FakeBot {
	return &w6FakeBot{nextMsg: 256, release: make(chan struct{})}
}

// approveData returns the Approve button's callback_data from the
// captured approval card (the card the prompter actually sent).
func (f *w6FakeBot) approveData() string { return f.buttonData("approve") }
func (f *w6FakeBot) denyData() string    { return f.buttonData("deny") }

func (f *w6FakeBot) buttonData(decision string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sent {
		markup, _ := s.Raw["reply_markup"].(map[string]any)
		rows, _ := markup["inline_keyboard"].([]any)
		for _, row := range rows {
			buttons, _ := row.([]any)
			for _, b := range buttons {
				btn, _ := b.(map[string]any)
				data, _ := btn["callback_data"].(string)
				if d, _, ok := ParsePermissionCallback(data); ok && d == decision {
					return data
				}
			}
		}
	}
	return ""
}

func (f *w6FakeBot) cardCaptured() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sent {
		if _, ok := s.Raw["reply_markup"]; ok {
			return true
		}
	}
	return false
}

func (f *w6FakeBot) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	writeOK := func(result string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"result":%s}`, result)
	}
	switch {
	case strings.HasSuffix(path, "/sendMessage"):
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		f.mu.Lock()
		f.nextMsg++
		msgID := f.nextMsg
		chatID, _ := raw["chat_id"].(float64)
		text, _ := raw["text"].(string)
		f.sent = append(f.sent, w6CapturedSend{ChatID: int64(chatID), Text: text, Raw: raw})
		f.mu.Unlock()
		writeOK(fmt.Sprintf(`{"message_id":%d}`, msgID))
	case strings.HasSuffix(path, "/sendChatAction"):
		f.mu.Lock()
		f.actions++
		f.mu.Unlock()
		writeOK(`true`)
	case strings.HasSuffix(path, "/answerCallbackQuery"):
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		f.mu.Lock()
		f.answers = append(f.answers, raw)
		f.mu.Unlock()
		writeOK(`true`)
	case strings.HasSuffix(path, "/editMessageText"):
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		f.mu.Lock()
		f.edits = append(f.edits, raw)
		f.mu.Unlock()
		writeOK(`{"message_id":257}`)
	case strings.HasSuffix(path, "/getUpdates"):
		f.mu.Lock()
		f.getCalls++
		call := f.getCalls
		f.mu.Unlock()
		offset := int64(0)
		fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
		if f.firstBatch != nil && call == 1 {
			writeOK(string(f.firstBatch))
			return
		}
		// The command message, once, when the offset allows it.
		if call == 1 && offset <= w6CmdUpd {
			writeOK(fmt.Sprintf(`[{"update_id":%d,"message":{"message_id":255,"from":{"id":%d,"is_bot":false,"first_name":"Irfan","username":"irfan","language_code":"en"},"chat":{"id":%d,"first_name":"Irfan","type":"private"},"date":1763030000,"text":"run shell_exec"}}]`, w6CmdUpd, w6ChatID, w6ChatID))
			return
		}
		// The tap(s): only once released, only when the offset
		// allows the update, one tap per getUpdates call.
		select {
		case <-f.release:
		default:
			time.Sleep(5 * time.Millisecond)
			writeOK(`[]`)
			return
		}
		f.mu.Lock()
		idx := f.tapIdx
		f.mu.Unlock()
		if idx >= len(f.taps) {
			time.Sleep(5 * time.Millisecond)
			writeOK(`[]`)
			return
		}
		tap := f.taps[idx]
		if offset > tap.UpdateID {
			time.Sleep(5 * time.Millisecond)
			writeOK(`[]`)
			return
		}
		data := tap.Data
		if data == "" {
			data = f.approveData()
		}
		if data == "PLACEHOLDER-DENY" {
			data = f.denyData()
		}
		// REAL Telegram callback_query shape: every extra field
		// the production payload carries (is_bot, first_name,
		// username, language_code, date, chat_instance, the
		// message's nested reply_markup) — the decode must survive
		// all of them.
		payload := fmt.Sprintf(`[{"update_id":%d,"callback_query":{"id":%q,"from":{"id":%d,"is_bot":false,"first_name":"Irfan","username":"irfan","language_code":"en"},"message":{"message_id":257,"from":{"id":123456,"is_bot":true,"first_name":"Magi","username":"Magi420x_bot"},"chat":{"id":%d,"first_name":"Irfan","type":"private"},"date":1763030100,"text":"Approval needed","reply_markup":{"inline_keyboard":[[{"text":"Approve once","callback_data":%q}]]}},"chat_instance":"-1234567890","data":%q}}]`,
			tap.UpdateID, tap.QueryID, w6ChatID, w6ChatID, data, data)
		f.mu.Lock()
		f.tapIdx++
		f.mu.Unlock()
		writeOK(payload)
	case strings.HasSuffix(path, "/setMyCommands"):
		writeOK(`true`)
	default:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"result":true}`)
	}
}

func (f *w6FakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) { f.handler(w, r) }

// w6Harness wires the REAL gate + prompter + expiry + callback
// handler + PollStream, exactly as oriond does, against the fake.
type w6Harness struct {
	fake    *w6FakeBot
	srv     *httptest.Server
	gw      *TelegramGateway
	gate    *permissions.Gate
	cards   *ApprovalCards
	execute atomic.Bool
	reply   atomic.Value // string, set by the turn handler
	denied  atomic.Value // string, set by the turn handler
	logs    []string
	logMu   sync.Mutex
}

func newW6Harness(t *testing.T, gateTimeout time.Duration) *w6Harness {
	t.Helper()
	h := &w6Harness{fake: newW6FakeBot()}
	h.srv = httptest.NewServer(h.fake)
	t.Cleanup(h.srv.Close)
	var err error
	h.gw, err = NewTelegramGateway("W6TESTTOKEN",
		WithBaseURL(h.srv.URL),
		WithAllowedChats(w6ChatID),
		WithLogger(func(format string, args ...any) {
			h.logMu.Lock()
			h.logs = append(h.logs, fmt.Sprintf(format, args...))
			h.logMu.Unlock()
		}))
	if err != nil {
		t.Fatal(err)
	}
	h.cards = NewApprovalCards()
	h.gate = permissions.NewGate(nil, ApprovalPrompter(h.gw, h.cards, "w6"))
	if gateTimeout > 0 {
		h.gate.SetTimeout(gateTimeout)
	}
	InstallApprovalExpiry(h.gate, h.gw, h.cards, "w6")
	InstallApprovalCallbackHandler(h.gw, h.gate, w6OwnerID, h.cards, "w6")
	return h
}

// turnHandler mirrors the live turn: approval context from the
// sender, then the gate, then (only if allowed) the "command".
func (h *w6Harness) turnHandler() TurnFunc {
	return func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		senderID, _ := SenderIDFromContext(ctx)
		ctx = permissions.WithApprovalContext(ctx, chatID, senderID)
		allowed, reason := h.gate.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
		if !allowed {
			h.denied.Store(reason)
			return "denied: " + reason, nil, nil
		}
		h.execute.Store(true)
		h.reply.Store("ran it")
		return "ran it", nil, nil
	}
}

func (h *w6Harness) runPoll(ctx context.Context, t *testing.T) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- h.gw.PollStream(ctx, h.turnHandler()) }()
	t.Cleanup(func() {
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("PollStream returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("PollStream did not return after context cancel")
		}
	})
}

// w6WaitFor polls cond until it is true or the deadline passes.
func w6WaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// capturedCard returns the sent approval card's raw body.
func (h *w6Harness) capturedCard(t *testing.T) w6CapturedSend {
	t.Helper()
	w6WaitFor(t, "approval card to be sent", h.fake.cardCaptured)
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, s := range h.fake.sent {
		if _, ok := s.Raw["reply_markup"]; ok {
			return s
		}
	}
	t.Fatal("no card captured")
	return w6CapturedSend{}
}

// assertCardKeyboard is the step-1 regression: the SENT card must
// carry a live inline keyboard whose every button has valid,
// parseable, ≤64-byte callback_data for the pending request.
func assertCardKeyboard(t *testing.T, card w6CapturedSend) (requestID string) {
	t.Helper()
	markup, ok := card.Raw["reply_markup"].(map[string]any)
	if !ok {
		t.Fatal("sent approval card has no reply_markup — Telegram would render a plain message with no buttons and a tap could never generate a callback_query")
	}
	rows, ok := markup["inline_keyboard"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("sent card inline_keyboard = %v, want 2 rows", markup["inline_keyboard"])
	}
	seen := map[string]bool{}
	for _, row := range rows {
		buttons, _ := row.([]any)
		for _, b := range buttons {
			btn, _ := b.(map[string]any)
			data, _ := btn["callback_data"].(string)
			if data == "" {
				t.Fatalf("sent card button %q has empty callback_data — a tap on it generates no callback_query", btn["text"])
			}
			if len(data) > 64 {
				t.Fatalf("sent card button %q callback_data is %d bytes, over Telegram's 64-byte limit — Telegram rejects the keyboard", btn["text"], len(data))
			}
			decision, id, ok := ParsePermissionCallback(data)
			if !ok {
				t.Fatalf("sent card button %q callback_data %q does not parse as perm:<decision>:<requestID>", btn["text"], data)
			}
			if seen[decision] {
				t.Fatalf("duplicate decision %q on sent card", decision)
			}
			seen[decision] = true
			if requestID == "" {
				requestID = id
			} else if id != requestID {
				t.Fatalf("sent card buttons carry different request IDs %q vs %q", requestID, id)
			}
		}
	}
	for _, d := range []string{"approve", "deny", "always", "never"} {
		if !seen[d] {
			t.Fatalf("sent card is missing the %q button", d)
		}
	}
	return requestID
}

func (h *w6Harness) answerWithText(text string) (string, bool) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, a := range h.fake.answers {
		if txt, _ := a["text"].(string); txt == text {
			return txt, true
		}
	}
	return "", false
}

// TestW6ApprovalApproveEndToEnd is the live shape: a turn blocked on
// gate.Check, the user taps Approve on the exact card that was sent,
// the callback_query (real Telegram JSON) arrives at the poller, the
// gate resolves, and the command runs — and it must NOT run before
// the tap.
func TestW6ApprovalApproveEndToEnd(t *testing.T) {
	h := newW6Harness(t, 0)
	h.fake.taps = []w6Tap{{UpdateID: w6CbUpd, QueryID: "tap-query-1"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runPoll(ctx, t)

	// The command message starts the turn; the gate sends the card.
	card := h.capturedCard(t)
	requestID := assertCardKeyboard(t, card)
	if card.ChatID != w6ChatID {
		t.Fatalf("card sent to chat %d, want %d", card.ChatID, w6ChatID)
	}

	// The command must NOT execute while the approval is pending —
	// the tap is the only thing that may release it.
	time.Sleep(100 * time.Millisecond)
	if h.execute.Load() {
		t.Fatal("command executed before approval: the gate did not block the turn")
	}

	// The user taps Approve on the card that was actually sent.
	close(h.fake.release)
	w6WaitFor(t, "command to execute after the tap", h.execute.Load)
	if got, _ := h.reply.Load().(string); got != "ran it" {
		t.Fatalf("turn reply = %q, want %q", got, "ran it")
	}
	// The tap was answered (spinner cleared) with the outcome.
	w6WaitFor(t, "answerCallbackQuery for the tap", func() bool {
		_, ok := h.answerWithText("✅ Approved — running it now.")
		return ok
	})
	// The card was edited in place to show the outcome, keyboard removed.
	w6WaitFor(t, "outcome edit of the card", func() bool {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		for _, e := range h.fake.edits {
			if msgID, _ := e["message_id"].(float64); int64(msgID) == 257 {
				if kb, ok := e["reply_markup"].(map[string]any); ok {
					if rows, _ := kb["inline_keyboard"].([]any); len(rows) == 0 {
						return true
					}
				}
			}
		}
		return false
	})
	// The poller logged the callback_query receipt (the line whose
	// absence is the live incident's signature).
	w6WaitFor(t, "callback_query receipt in the poll log", func() bool {
		h.logMu.Lock()
		defer h.logMu.Unlock()
		for _, l := range h.logs {
			if strings.Contains(l, "callback_query") && strings.Contains(l, "181487685") {
				return true
			}
		}
		return false
	})
	_ = requestID
}

// TestW6ApprovalDenyEndToEnd: a Deny tap denies and nothing runs.
func TestW6ApprovalDenyEndToEnd(t *testing.T) {
	h := newW6Harness(t, 0)
	// The deny data comes from the card itself; the fake resolves
	// the placeholder against the captured card at serve time.
	h.fake.taps = []w6Tap{{UpdateID: w6CbUpd, QueryID: "tap-query-deny", Data: "PLACEHOLDER-DENY"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runPoll(ctx, t)
	card := h.capturedCard(t)
	assertCardKeyboard(t, card)
	close(h.fake.release)
	w6WaitFor(t, "denial to be recorded", func() bool {
		_, ok := h.denied.Load().(string)
		return ok
	})
	if h.execute.Load() {
		t.Fatal("command executed after a Deny tap")
	}
	w6WaitFor(t, "answerCallbackQuery for the deny tap", func() bool {
		_, ok := h.answerWithText("❌ Denied.")
		return ok
	})
}

// TestW6ApprovalExpired: no tap arrives; the request times out, the
// command never runs, and the expiry edit strips the keyboard so a
// late tap can never resolve a dead card.
func TestW6ApprovalExpired(t *testing.T) {
	h := newW6Harness(t, 500*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runPoll(ctx, t)
	card := h.capturedCard(t)
	assertCardKeyboard(t, card)
	w6WaitFor(t, "denial to be recorded", func() bool {
		_, ok := h.denied.Load().(string)
		return ok
	})
	if h.execute.Load() {
		t.Fatal("command executed after approval timed out")
	}
	if got, _ := h.denied.Load().(string); !strings.Contains(got, "timed out") {
		t.Fatalf("denial reason = %q, want it to mention the timeout", got)
	}
	// Expiry edit: same card, text replaced, keyboard removed.
	w6WaitFor(t, "expiry edit stripping the keyboard", func() bool {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		for _, e := range h.fake.edits {
			if msgID, _ := e["message_id"].(float64); int64(msgID) == 257 {
				if kb, ok := e["reply_markup"].(map[string]any); ok {
					if rows, _ := kb["inline_keyboard"].([]any); len(rows) == 0 {
						if txt, _ := e["text"].(string); strings.Contains(txt, "Expired") {
							return true
						}
					}
				}
			}
		}
		return false
	})
}

// TestW6ApprovalDuplicateTap: two taps on Approve (a double-click)
// resolve the request exactly once; the second tap is told the
// request has expired.
func TestW6ApprovalDuplicateTap(t *testing.T) {
	h := newW6Harness(t, 0)
	h.fake.taps = []w6Tap{
		{UpdateID: w6CbUpd, QueryID: "tap-query-1"},
		{UpdateID: w6SecondCb, QueryID: "tap-query-2"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runPoll(ctx, t)
	card := h.capturedCard(t)
	assertCardKeyboard(t, card)
	close(h.fake.release)
	w6WaitFor(t, "command to execute after the first tap", h.execute.Load)
	w6WaitFor(t, "second tap to be answered as expired", func() bool {
		_, ok := h.answerWithText("This approval request has expired.")
		return ok
	})
	// Exactly one approval outcome answer: the request resolved once.
	count := 0
	h.fake.mu.Lock()
	for _, a := range h.fake.answers {
		if txt, _ := a["text"].(string); txt == "✅ Approved — running it now." {
			count++
		}
	}
	h.fake.mu.Unlock()
	if count != 1 {
		t.Fatalf("approval outcome answered %d times, want exactly 1 (duplicate tap resolved twice)", count)
	}
}

// TestW6ApprovalStaleCallback: a tap carrying an unknown request ID
// is answered "expired" and resolves nothing.
func TestW6ApprovalStaleCallback(t *testing.T) {
	h := newW6Harness(t, 0)
	h.fake.taps = []w6Tap{{UpdateID: w6CbUpd, QueryID: "tap-query-stale", Data: "perm:approve:0000000000000000"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runPoll(ctx, t)
	card := h.capturedCard(t)
	assertCardKeyboard(t, card)
	close(h.fake.release)
	w6WaitFor(t, "stale tap to be answered as expired", func() bool {
		_, ok := h.answerWithText("This approval request has expired.")
		return ok
	})
	// The real request is still pending (short wait), so nothing ran
	// and no approval outcome was answered.
	time.Sleep(100 * time.Millisecond)
	if h.execute.Load() {
		t.Fatal("stale callback executed the command")
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, a := range h.fake.answers {
		if txt, _ := a["text"].(string); txt == "✅ Approved — running it now." {
			t.Fatal("stale callback produced an approval outcome")
		}
	}
}

// TestW6CallbackSameBatchAsMessage: a callback_query arriving in the
// SAME getUpdates batch as a message must still reach its handler —
// the original (b) hypothesis (a same-batch tap skipped by the
// offset/dispatch logic).
func TestW6CallbackSameBatchAsMessage(t *testing.T) {
	var cbCount atomic.Int32
	var msgCount atomic.Int32
	fake := newW6FakeBot()
	// One batch: callback first, message second (and the reverse
	// order is covered by the two-pass dispatch — the callback is
	// dispatched before messages regardless of batch order).
	fake.firstBatch = []byte(fmt.Sprintf(`[{"update_id":%d,"callback_query":{"id":"tap-same-batch","from":{"id":%d,"is_bot":false,"first_name":"Irfan"},"message":{"message_id":257,"from":{"id":123456,"is_bot":true},"chat":{"id":%d,"type":"private"},"date":1763030100,"text":"Approval needed"},"chat_instance":"x","data":"perm:approve:aaaaaaaaaaaaaaaa"}},{"update_id":%d,"message":{"message_id":258,"from":{"id":%d,"is_bot":false,"first_name":"Irfan"},"chat":{"id":%d,"type":"private"},"date":1763030101,"text":"hello"}}]`,
		w6CbUpd, w6ChatID, w6ChatID, w6CmdUpd, w6ChatID, w6ChatID))
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway("W6TESTTOKEN", WithBaseURL(srv.URL), WithAllowedChats(w6ChatID))
	if err != nil {
		t.Fatal(err)
	}
	gw.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		cbCount.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- gw.PollStream(ctx, func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
			msgCount.Add(1)
			return "ok", nil, nil
		})
	}()
	w6WaitFor(t, "callback handler to run", func() bool { return cbCount.Load() == 1 })
	w6WaitFor(t, "message handler to run", func() bool { return msgCount.Load() == 1 })
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("PollStream error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PollStream did not return after cancel")
	}
}
