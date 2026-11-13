package messaging

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// --- Sender check + owner tier + callbacks (P1-O4) -----------------------

func TestIsSenderAllowed(t *testing.T) {
	g, err := NewTelegramGateway(testToken, WithAllowedChats(77, -100))
	if err != nil {
		t.Fatal(err)
	}
	// Private chat: the allowlisted user passes.
	if !g.IsSenderAllowed(&TGUser{ID: 77}) {
		t.Error("allowlisted sender 77 refused")
	}
	// A stranger is refused, even in an allowlisted group chat.
	if g.IsSenderAllowed(&TGUser{ID: 5}) {
		t.Error("non-allowlisted sender 5 allowed")
	}
	// No sender (channel post, anonymous admin) is never allowed.
	if g.IsSenderAllowed(nil) {
		t.Error("nil sender allowed")
	}
	if g.IsSenderAllowed(&TGUser{ID: 0}) {
		t.Error("zero sender ID allowed")
	}
	// No allowlist at all: any sender passes (the open-gateway
	// rule), but a nil sender still does not.
	open, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatal(err)
	}
	if !open.IsSenderAllowed(&TGUser{ID: 12345}) {
		t.Error("open gateway refused a sender")
	}
	if open.IsSenderAllowed(nil) {
		t.Error("open gateway allowed a nil sender")
	}
}

func TestOwnerIDFromAllowlist(t *testing.T) {
	if got := OwnerIDFromAllowlist([]int64{7429947930, 77}); got != 7429947930 {
		t.Errorf("owner = %d, want the first entry 7429947930", got)
	}
	if got := OwnerIDFromAllowlist(nil); got != 0 {
		t.Errorf("owner of empty allowlist = %d, want 0", got)
	}
	if got := OwnerIDFromAllowlist([]int64{0, 5}); got != 5 {
		t.Errorf("owner = %d, want 5 (zero entries skipped)", got)
	}
}

func TestParsePermissionCallback(t *testing.T) {
	dec, id, ok := ParsePermissionCallback("perm:approve:abcdef0123456789")
	if !ok || dec != "approve" || id != "abcdef0123456789" {
		t.Errorf("parse approve = (%q, %q, %v), want (approve, abcdef0123456789, true)", dec, id, ok)
	}
	for _, data := range []string{
		"perm:always:0011223344556677",
		"perm:never:0011223344556677",
		"perm:deny:0011223344556677",
	} {
		if _, _, ok := ParsePermissionCallback(data); !ok {
			t.Errorf("ParsePermissionCallback(%q) = not ok, want ok", data)
		}
	}
	for _, data := range []string{
		"",
		"approve:abc",
		"perm:maybe:abc",
		"perm:approve:",
		"perm:approve",
		"perm::abc",
		"other:approve:abc",
	} {
		if _, _, ok := ParsePermissionCallback(data); ok {
			t.Errorf("ParsePermissionCallback(%q) = ok, want not ok", data)
		}
	}
}

func TestApprovalKeyboardRoundTrip(t *testing.T) {
	kb := ApprovalKeyboard("0123456789abcdef")
	if len(kb.InlineKeyboard) != 2 {
		t.Fatalf("keyboard rows = %d, want 2", len(kb.InlineKeyboard))
	}
	if len(kb.InlineKeyboard[0]) != 2 || len(kb.InlineKeyboard[1]) != 2 {
		t.Fatalf("keyboard buttons = %v, want 2+2", kb.InlineKeyboard)
	}
	seen := map[string]bool{}
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			dec, id, ok := ParsePermissionCallback(b.CallbackData)
			if !ok || id != "0123456789abcdef" {
				t.Errorf("button %q: callback data %q does not parse", b.Text, b.CallbackData)
			}
			seen[dec] = true
		}
	}
	for _, want := range []string{"approve", "deny", "always", "never"} {
		if !seen[want] {
			t.Errorf("keyboard missing the %q decision", want)
		}
	}
}

// TestPollDropsNonAllowlistedSender: in an allowlisted group chat,
// only allowlisted members may drive the bot. A stranger's message
// in the group is dropped before the handler; the allowlisted
// member's message is delivered.
func TestPollDropsNonAllowlistedSender(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: -100}, Text: "stranger", From: &TGUser{ID: 5}}},
			{UpdateID: 2, Message: &TGMessage{MessageID: 2, Chat: TGChat{ID: -100}, Text: "owner here", From: &TGUser{ID: 77}}},
			{UpdateID: 3, Message: &TGMessage{MessageID: 3, Chat: TGChat{ID: -100}, Text: "no sender"}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77, -100))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var handled []string
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		mu.Lock()
		handled = append(handled, text)
		mu.Unlock()
		return "ack", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 1 || handled[0] != "owner here" {
		t.Errorf("handled = %v, want only [owner here]", handled)
	}
}

// TestPollHandlerSeesSenderID: the turn handler's context carries
// the sender user ID (P1-O4), so the daemon can enforce the owner
// tier and the permission gate can bind approvals to the requester.
func TestPollHandlerSeesSenderID(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	saw := make(chan int64, 1)
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		id, _ := SenderIDFromContext(ctx)
		saw <- id
		return "ack", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()
	select {
	case id := <-saw:
		if id != 77 {
			t.Errorf("sender in handler ctx = %d, want 77", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never called")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after cancel")
	}
}

// TestCallbackQueryReachesHandler: a callback query update is
// delivered to the installed callback handler (with its sender in
// context), and the query is answered so the button spinner clears.
func TestCallbackQueryReachesHandler(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, CallbackQuery: &TGCallbackQuery{
			ID:      "cb-1",
			From:    &TGUser{ID: 77},
			Message: &TGMessage{MessageID: 9, Chat: TGChat{ID: 77}},
			Data:    "perm:approve:0123456789abcdef",
		}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	var mu sync.Mutex
	var got []TGCallbackQuery
	var gotSender int64
	g.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		mu.Lock()
		got = append(got, q)
		id, _ := SenderIDFromContext(ctx)
		gotSender = id
		mu.Unlock()
		// A real handler answers the query (clearing the button
		// spinner); the gateway does not answer for it.
		return g.AnswerCallbackQuery(ctx, q.ID, "Approved")
	})
	cancel, errCh := runPollStream(t, g, func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "unexpected turn", nil, nil
	})
	defer stopPollStream(t, cancel, errCh)
	waitFor(t, "callback delivery", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	gotData, gotSenderID := got[0].Data, gotSender
	mu.Unlock()
	if gotData != "perm:approve:0123456789abcdef" {
		t.Errorf("callback data = %q, want the approval payload", gotData)
	}
	if gotSenderID != 77 {
		t.Errorf("callback sender in ctx = %d, want 77", gotSenderID)
	}
	waitFor(t, "callback answer", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.answered) >= 1
	})
}

// TestCallbackQueryFromStrangerDropped: a tap from a non-allowlisted
// user never reaches the callback handler, so a stranger can never
// approve a pending tool call.
func TestCallbackQueryFromStrangerDropped(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, CallbackQuery: &TGCallbackQuery{
			ID:      "cb-1",
			From:    &TGUser{ID: 5},
			Message: &TGMessage{MessageID: 9, Chat: TGChat{ID: 77}},
			Data:    "perm:approve:0123456789abcdef",
		}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	g.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error {
		called <- struct{}{}
		return nil
	})
	cancel, errCh := runPollStream(t, g, func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "unexpected turn", nil, nil
	})
	defer stopPollStream(t, cancel, errCh)
	select {
	case <-called:
		t.Fatal("callback handler was called for a non-allowlisted tapper")
	case <-time.After(500 * time.Millisecond):
		// Not called: correct.
	}
}

// TestSendMessageWithKeyboard: the approval prompt transport sends
// reply_markup with the keyboard, and AnswerCallbackQuery answers.
func TestSendMessageWithKeyboard(t *testing.T) {
	fake := &streamFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)
	ctx := context.Background()
	id, err := g.SendMessageWithKeyboard(ctx, 77, "approve?", ApprovalKeyboard("0123456789abcdef"))
	if err != nil {
		t.Fatalf("SendMessageWithKeyboard: %v", err)
	}
	if id == 0 {
		t.Error("SendMessageWithKeyboard returned message id 0")
	}
	if err := g.AnswerCallbackQuery(ctx, "cb-1", "Approved"); err != nil {
		t.Fatalf("AnswerCallbackQuery: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.answered) != 1 || fake.answered[0] != "cb-1" {
		t.Errorf("answered callbacks = %v, want [cb-1]", fake.answered)
	}
	// The keyboard must have reached the fake server: streamFake
	// records the raw sendMessage body for keyboard sends.
	found := false
	for _, kb := range fake.keyboards {
		if strings.Contains(kb, "0123456789abcdef") && strings.Contains(kb, "Approve once") {
			found = true
		}
	}
	if !found {
		t.Errorf("no keyboard with the approval buttons reached the server: %v", fake.keyboards)
	}
}

// TestGetUpdatesRealTelegramCallbackPayload: the live rc8 incident
// (Irfan tapped Approve, gate timed out, zero callback lines) could
// in principle be a real-Telegram JSON shape the structs mishandle
// (extra fields, nested reply_markup, chat_instance). Feed a
// realistic raw Bot API payload — verbatim, exactly as Telegram
// sends it, with all the fields a real callback_query carries —
// through the real GetUpdates and assert the callback query parses
// with the right data, sender and message. Unknown fields must be
// ignored; known fields must land in the right struct fields.
func TestGetUpdatesRealTelegramCallbackPayload(t *testing.T) {
	raw := `{"ok":true,"result":[{"update_id":987654321,"callback_query":{"id":"1122334455667788990","from":{"id":7429947930,"is_bot":false,"first_name":"Irfan","username":"irfan420x","language_code":"en"},"message":{"message_id":456,"from":{"id":123456789,"is_bot":true,"first_name":"ORION","username":"Magi420x_bot"},"chat":{"id":7429947930,"first_name":"Irfan","type":"private"},"date":1763013360,"text":"⚠️ Approval needed\n\nshell_exec: echo hi","reply_markup":{"inline_keyboard":[[{"text":"✅ Approve once","callback_data":"perm:approve:abcdef0123456789"},{"text":"❌ Deny","callback_data":"perm:deny:abcdef0123456789"}]]}},"chat_instance":"-9876543210987654321","data":"perm:approve:abcdef0123456789"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, raw)
	}))
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	updates, err := gw.GetUpdates(context.Background())
	if err != nil {
		t.Fatalf("GetUpdates on a realistic Telegram payload: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(updates))
	}
	q := updates[0].CallbackQuery
	if q == nil {
		t.Fatal("CallbackQuery is nil for a real callback_query payload")
	}
	if q.ID != "1122334455667788990" {
		t.Errorf("callback id = %q, want %q", q.ID, "1122334455667788990")
	}
	if q.Data != "perm:approve:abcdef0123456789" {
		t.Errorf("callback data = %q, want the perm:approve payload", q.Data)
	}
	if q.From == nil || q.From.ID != 7429947930 {
		t.Errorf("callback from = %v, want user 7429947930", q.From)
	}
	if q.Message == nil || q.Message.MessageID != 456 || q.Message.Chat.ID != 7429947930 {
		t.Errorf("callback message = %+v, want message 456 in chat 7429947930", q.Message)
	}
	dec, id, ok := ParsePermissionCallback(q.Data)
	if !ok || dec != "approve" || id != "abcdef0123456789" {
		t.Errorf("ParsePermissionCallback(%q) = (%q, %q, %v), want (approve, abcdef0123456789, true)", q.Data, dec, id, ok)
	}
	if got := gw.Offset(); got != 987654322 {
		t.Errorf("gateway offset after the callback update = %d, want 987654322 (maxID+1)", got)
	}
}

// TestEditMessageTextMarkupRemovesKeyboard: an expired approval card
// must lose its buttons (the rc8 incident: a dead card with live
// buttons looks exactly like a broken button). EditMessageTextMarkup
// with an empty InlineKeyboardMarkup sends reply_markup with an
// empty inline_keyboard, which is Telegram's way to remove the
// keyboard.
func TestEditMessageTextMarkupRemovesKeyboard(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.EditMessageTextMarkup(context.Background(), 77, 9, "expired", InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}); err != nil {
		t.Fatalf("EditMessageTextMarkup: %v", err)
	}
	if !strings.Contains(gotBody, `"reply_markup":{"inline_keyboard":[]}`) {
		t.Errorf("edit body = %s, want reply_markup with an empty inline_keyboard", gotBody)
	}
	if !strings.Contains(gotBody, `"text":"expired"`) {
		t.Errorf("edit body = %s, want the expiry text", gotBody)
	}
}
