// Shared permission-gate Telegram wiring tests (F-A7-P2-2).
//
// Before rc12 the approval wiring existed twice — cmd/orion's
// wireGate and cmd/oriond's buildApprovalGate — as two
// near-identical copies. Both binaries now use the shared
// implementation in telegram.go (ApprovalPrompter,
// InstallApprovalExpiry, HandleApprovalCallback,
// InstallApprovalCallbackHandler) and build the gate through the
// shared constructor (app.NewPermissionGate). These tests prove
// the shared wiring itself: the prompter sends a card and tracks
// it, the expiry hook edits a dead card in place, the installed
// callback handler (not a direct call) resolves a tap delivered
// through PollStream, and a stranger's tap is refused. The
// per-binary tests (cmd/orion, cmd/oriond) prove each binary
// actually installs this wiring.
package messaging

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/permissions"
)

// approvalCardID scans the fake's sent messages for the first
// approval card for chatID and returns its request ID and card
// message ID, or ("", 0) when no card has arrived yet.
func approvalCardID(fake *queueFake, chatID int64) (string, int64) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, s := range fake.sent {
		if s.ChatID != chatID || !strings.Contains(s.Keyboard, "perm:approve:") {
			continue
		}
		var kb InlineKeyboardMarkup
		if err := json.Unmarshal([]byte(s.Keyboard), &kb); err != nil || len(kb.InlineKeyboard) == 0 || len(kb.InlineKeyboard[0]) == 0 {
			continue
		}
		if _, id, ok := ParsePermissionCallback(kb.InlineKeyboard[0][0].CallbackData); ok {
			return id, s.MsgID
		}
	}
	return "", 0
}

// TestSharedApprovalWiringPromptsAndResolvesViaInstalledHandler
// proves the shared wiring end to end through PollStream: a
// shell_exec Check sends an approval card through the shared
// prompter, the card is tracked, and a tap delivered by
// getUpdates reaches the INSTALLED callback handler (set by
// InstallApprovalCallbackHandler) and resolves the Check. It
// fails if either binary's wiring stopped installing the handler
// — the gate would prompt but no tap could ever answer it.
func TestSharedApprovalWiringPromptsAndResolvesViaInstalledHandler(t *testing.T) {
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	cards := NewApprovalCards()
	gate := permissions.NewGate(permissions.NewRuleStore(""), ApprovalPrompter(gw, cards, "test"))
	InstallApprovalExpiry(gate, gw, cards, "test")
	InstallApprovalCallbackHandler(gw, gate, 77, cards, nil, "test")
	if gw.callbackHandlerFor() == nil {
		t.Fatal("InstallApprovalCallbackHandler did not install a callback handler on the gateway")
	}

	turnHandler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "ok", nil, nil
	}
	cancel, errCh := runPollStream(t, gw, turnHandler)
	defer stopPollStream(t, cancel, errCh)

	ctx := permissions.WithApprovalContext(context.Background(), 77, 77)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := gate.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var reqID string
	var cardMsgID int64
	waitFor(t, "approval card for chat 77", func() bool {
		reqID, cardMsgID = approvalCardID(fake, 77)
		return reqID != ""
	})
	if cardMsgID == 0 {
		t.Fatal("approval card arrived with no message id")
	}
	// The card is tracked: the tracker holds this request's card.
	if msgID, ok := cards.Take(reqID); !ok || msgID != cardMsgID {
		t.Fatalf("cards.Take(%s) = (%d, %v), want (%d, true)", reqID, msgID, ok, cardMsgID)
	}
	// Put it back: Take removed it, and the tap below must still
	// resolve through the same tracker.
	cards.Note(reqID, cardMsgID)

	fake.enqueue(Update{UpdateID: 1, CallbackQuery: &TGCallbackQuery{
		ID:      "cb-shared-approve",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: cardMsgID, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:" + reqID,
	}})
	select {
	case ok := <-resultCh:
		if !ok {
			t.Error("Check returned false after the requester's approve tap, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the approve tap delivered through PollStream")
	}
	if _, found := gate.Pending(reqID); found {
		t.Error("request still pending after the approve tap resolved it")
	}
	waitFor(t, "card edited with the outcome", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, e := range fake.edits {
			if e.MsgID == cardMsgID && strings.Contains(e.Text, "Approved") {
				return true
			}
		}
		return false
	})
	waitFor(t, "tap answered", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, id := range fake.answered {
			if id == "cb-shared-approve" {
				return true
			}
		}
		return false
	})
}

// TestSharedApprovalCallbackRejectsStranger proves the shared tap
// resolver's authorisation rule at unit level: a stranger's tap
// is refused and the request stays pending; the requester's tap
// then approves it. A tap on an unknown (expired) request is
// answered as expired, and a nil card tracker never panics.
func TestSharedApprovalCallbackRejectsStranger(t *testing.T) {
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	cards := NewApprovalCards()
	gate := permissions.NewGate(permissions.NewRuleStore(""), ApprovalPrompter(gw, cards, "test"))

	ctx := permissions.WithApprovalContext(context.Background(), 77, 77)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := gate.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var reqID string
	var cardMsgID int64
	waitFor(t, "approval card for chat 77", func() bool {
		reqID, cardMsgID = approvalCardID(fake, 77)
		return reqID != ""
	})

	stranger := TGCallbackQuery{
		ID:      "cb-stranger",
		From:    &TGUser{ID: 999},
		Message: &TGMessage{MessageID: cardMsgID, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:" + reqID,
	}
	if err := HandleApprovalCallback(context.Background(), gw, gate, 77, stranger, cards, "test"); err != nil {
		t.Fatalf("stranger tap: %v", err)
	}
	if gate.PendingCount() != 1 {
		t.Errorf("PendingCount after stranger tap = %d, want 1 (the request must stay pending)", gate.PendingCount())
	}
	select {
	case <-resultCh:
		t.Fatal("stranger's tap resolved the approval")
	default:
	}

	approve := TGCallbackQuery{
		ID:      "cb-approve",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: cardMsgID, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:" + reqID,
	}
	// A nil tracker is "no tracking", not a panic: the tap still
	// resolves, it just records nothing.
	if err := HandleApprovalCallback(context.Background(), gw, gate, 77, approve, nil, "test"); err != nil {
		t.Fatalf("requester tap with nil cards: %v", err)
	}
	select {
	case ok := <-resultCh:
		if !ok {
			t.Error("Check returned false after the requester's approve tap, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the approve tap")
	}

	// A tap on an unknown request is answered as expired.
	expired := TGCallbackQuery{
		ID:      "cb-expired",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: cardMsgID, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:0123456789abcdef",
	}
	if err := HandleApprovalCallback(context.Background(), gw, gate, 77, expired, cards, "test"); err != nil {
		t.Fatalf("expired tap: %v", err)
	}
	waitFor(t, "expired tap answered", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, id := range fake.answered {
			if id == "cb-expired" {
				return true
			}
		}
		return false
	})
}

// TestSharedApprovalExpiryEditsCard proves the shared expiry hook:
// when a prompt dies unanswered, its card is edited in place with
// the expiry text and an empty keyboard, so nobody can tap a dead
// button.
func TestSharedApprovalExpiryEditsCard(t *testing.T) {
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	cards := NewApprovalCards()
	gate := permissions.NewGate(permissions.NewRuleStore(""), ApprovalPrompter(gw, cards, "test"))
	gate.SetTimeout(150 * time.Millisecond)
	InstallApprovalExpiry(gate, gw, cards, "test")

	ctx := permissions.WithApprovalContext(context.Background(), 77, 77)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := gate.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var reqID string
	var cardMsgID int64
	waitFor(t, "approval card for chat 77", func() bool {
		reqID, cardMsgID = approvalCardID(fake, 77)
		return reqID != ""
	})
	select {
	case ok := <-resultCh:
		if ok {
			t.Error("Check returned true for an unanswered request, want false (expired)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the approval window")
	}
	waitFor(t, "expired card edited in place", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, e := range fake.edits {
			if e.MsgID == cardMsgID && strings.Contains(e.Text, "Expired") && e.HasKeys {
				return true
			}
		}
		return false
	})
}

// TestApprovalCardsTrackTakeForget proves the shared card tracker
// semantics: Note records, Take removes exactly once, Forget
// removes without returning, and a nil tracker is safe to call.
func TestApprovalCardsTrackTakeForget(t *testing.T) {
	var nilCards *ApprovalCards
	nilCards.Note("x", 1)
	if _, ok := nilCards.Take("x"); ok {
		t.Error("nil tracker Take returned a card, want none")
	}
	nilCards.Forget("x")

	cards := NewApprovalCards()
	cards.Note("a", 11)
	cards.Note("b", 22)
	if msgID, ok := cards.Take("a"); !ok || msgID != 11 {
		t.Errorf("Take(a) = (%d, %v), want (11, true)", msgID, ok)
	}
	if _, ok := cards.Take("a"); ok {
		t.Error("Take(a) twice returned a card, want the second take to miss")
	}
	cards.Forget("b")
	if _, ok := cards.Take("b"); ok {
		t.Error("Take(b) after Forget returned a card, want a miss")
	}
}
