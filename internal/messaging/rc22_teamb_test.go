package messaging

// rc22 Team B.
//
// F-A16-2 — allowed_updates: the gateway sends
// allowed_updates=["message","callback_query"] on its first
// successful getUpdates, resetting any stale server-side filter
// (the rc8 incident: a persisted ["message"] filter meant
// callback_query was never delivered and approval buttons silently
// died). The filter rides on the ordinary first long poll — no
// extra round-trip, no update consumed — and is retried on the
// next fetch when the first fetch fails, so a failure can never
// fail startup or stop polling.
//
// F-A16-3 — per-chat rules: the approval card says Always/Never
// applies to this chat only, and tapping "Always" in one chat
// persists a rule for that chat only.

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

func TestRC22AllowedUpdatesSentOnceOnFirstFetch(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	updates, err := gw.GetUpdates(ctx)
	if err != nil {
		t.Fatalf("first GetUpdates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("first GetUpdates returned %d updates, want 1", len(updates))
	}
	for i := 0; i < 2; i++ {
		if _, err := gw.GetUpdates(ctx); err != nil {
			t.Fatalf("GetUpdates %d: %v", i+2, err)
		}
	}
	queries := fake.getUpdatesQueriesSnapshot()
	if len(queries) != 3 {
		t.Fatalf("getUpdates queries = %v, want 3 calls", queries)
	}
	if queries[0] != `["message","callback_query"]` {
		t.Errorf("first getUpdates allowed_updates = %q, want %q", queries[0], `["message","callback_query"]`)
	}
	if queries[1] != "" || queries[2] != "" {
		t.Errorf("later getUpdates calls carried allowed_updates: %v, want only the first to carry it", queries)
	}
}

// TestRC22AllowedUpdatesRetriedUntilFetchSucceeds: when the first
// fetches fail, the filter is NOT treated as sent — it rides on
// the next fetch, and the fetch after the first success carries no
// filter. A failed reset therefore never loses the subscription
// and never stops polling.
func TestRC22AllowedUpdatesRetriedUntilFetchSucceeds(t *testing.T) {
	fake := &streamFake{
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
		},
		failGetUpdates: 2,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := gw.GetUpdates(ctx); err == nil {
			t.Errorf("GetUpdates %d: expected an error, got nil", i+1)
		}
	}
	updates, err := gw.GetUpdates(ctx)
	if err != nil {
		t.Fatalf("third GetUpdates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("third GetUpdates returned %d updates, want 1", len(updates))
	}
	if _, err := gw.GetUpdates(ctx); err != nil {
		t.Fatalf("fourth GetUpdates: %v", err)
	}
	queries := fake.getUpdatesQueriesSnapshot()
	if len(queries) != 4 {
		t.Fatalf("getUpdates queries = %v, want 4 calls", queries)
	}
	for i := 0; i < 3; i++ {
		if queries[i] != `["message","callback_query"]` {
			t.Errorf("getUpdates %d allowed_updates = %q, want the filter (retried until success)", i+1, queries[i])
		}
	}
	if queries[3] != "" {
		t.Errorf("fourth getUpdates allowed_updates = %q, want empty (filter already sent)", queries[3])
	}
}

// TestRC22AllowedUpdatesSentOnceViaPollStream: through the real
// polling loop the filter is sent exactly once, on the first
// fetch, however many polls follow.
func TestRC22AllowedUpdatesSentOnceViaPollStream(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 1)
	cancel, errCh := runPollStream(t, gw, func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		select {
		case received <- struct{}{}:
		default:
		}
		return "ok", nil, nil
	})
	defer stopPollStream(t, cancel, errCh)
	waitFor(t, "turn for the scripted update", func() bool {
		select {
		case <-received:
			return true
		default:
			return false
		}
	})
	// Let the loop poll several more times, then count while it
	// is still running (the deferred stop ends it): the filter
	// count must stay at exactly one however many polls follow.
	waitFor(t, "at least 4 getUpdates calls", func() bool {
		return len(fake.getUpdatesQueriesSnapshot()) >= 4
	})
	queries := fake.getUpdatesQueriesSnapshot()
	filtered := 0
	for _, q := range queries {
		if q != "" {
			filtered++
			if q != `["message","callback_query"]` {
				t.Errorf("filtered getUpdates allowed_updates = %q, want [message callback_query]", q)
			}
		}
	}
	if filtered != 1 {
		t.Errorf("getUpdates calls carrying allowed_updates = %d, want exactly 1 (queries: %v)", filtered, queries)
	}
	if len(queries) == 0 || queries[0] != `["message","callback_query"]` {
		t.Errorf("first getUpdates query = %v, want it to carry the filter", queries)
	}
}

// TestRC22PollStreamSurvivesFailedFilterReset: the first fetch
// fails (the filter never lands), so PollStream must back off and
// keep polling — the update still arrives, the filter is retried
// on the successful fetch, and PollStream never returns an error.
func TestRC22PollStreamSurvivesFailedFilterReset(t *testing.T) {
	fake := &streamFake{
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
		},
		failGetUpdates: 1,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 1)
	cancel, errCh := runPollStream(t, gw, func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		select {
		case received <- struct{}{}:
		default:
		}
		return "ok", nil, nil
	})
	defer stopPollStream(t, cancel, errCh)
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("update never delivered after the failed first fetch — polling died with the filter reset")
	}
	queries := fake.getUpdatesQueriesSnapshot()
	filtered := 0
	for _, q := range queries {
		if q != "" {
			filtered++
		}
	}
	// The failed first fetch and the successful retry both carry
	// the filter; later polls carry none.
	if filtered != 2 {
		t.Errorf("getUpdates calls carrying allowed_updates = %d, want 2 (failed first + successful retry) (queries: %v)", filtered, queries)
	}
}

// TestRC22ApprovalCardStatesPerChatScope: the card text and the
// Always/Never outcome both say the remembered choice applies to
// this chat only — and tapping "Always" through the real callback
// path persists a rule for that chat only.
func TestRC22ApprovalCardStatesPerChatScope(t *testing.T) {
	req := permissions.ApprovalRequest{ID: "0123456789abcdef", ChatID: 77, SenderID: 77, Tool: "shell_exec", Arguments: `{"command":"echo hi"}`}
	text := ApprovalPromptText(req)
	if !strings.Contains(text, "in this chat only") {
		t.Errorf("approval card text = %q, want it to say Always/Never applies to this chat only", text)
	}

	fake := &streamFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	rules := permissions.NewRuleStore("")
	cards := NewApprovalCards()
	gate := permissions.NewGate(rules, ApprovalPrompter(gw, cards, "test"))
	ctx := permissions.WithApprovalContext(context.Background(), 77, 77)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := gate.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	// Find the card the prompter sent and its request ID.
	var reqID string
	var cardMsgID int64
	waitFor(t, "approval card for chat 77", func() bool {
		sent, _, _ := fake.snapshot()
		for _, s := range sent {
			if s.ChatID != 77 || !strings.Contains(s.Text, "in this chat only") {
				continue
			}
			for _, kb := range fake.keyboardsSnapshot() {
				var markup InlineKeyboardMarkup
				var body struct {
					ReplyMarkup InlineKeyboardMarkup `json:"reply_markup"`
				}
				if err := json.Unmarshal([]byte(kb), &body); err != nil {
					continue
				}
				markup = body.ReplyMarkup
				for _, row := range markup.InlineKeyboard {
					for _, b := range row {
						if _, id, ok := ParsePermissionCallback(b.CallbackData); ok {
							reqID, cardMsgID = id, s.MsgID
							return true
						}
					}
				}
			}
		}
		return false
	})
	if reqID == "" || cardMsgID == 0 {
		t.Fatal("approval card never arrived")
	}
	if err := HandleApprovalCallback(context.Background(), gw, gate, 77, TGCallbackQuery{
		ID:      "cb-rc22-always",
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: cardMsgID, Chat: TGChat{ID: 77}},
		Data:    "perm:always:" + reqID,
	}, cards, "test"); err != nil {
		t.Fatalf("HandleApprovalCallback: %v", err)
	}
	select {
	case ok := <-resultCh:
		if !ok {
			t.Error("Check returned false after the Always tap, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the Always tap")
	}
	// The rule is for chat 77 only.
	if a, ok := rules.LookupForChat(77, "shell_exec"); !ok || a != permissions.ActionAllow {
		t.Errorf("chat 77 rule = %q/%v, want allow/true", a, ok)
	}
	if _, ok := rules.LookupForChat(78, "shell_exec"); ok {
		t.Error("Always tapped in chat 77 leaked a rule into chat 78")
	}
	// The outcome edit on the card says "in this chat".
	_, edits, _ := fake.snapshot()
	found := false
	for _, e := range edits {
		if e.MessageID == cardMsgID && strings.Contains(e.Text, "in this chat") {
			found = true
		}
	}
	if !found {
		t.Errorf("no outcome edit says the rule is for this chat (edits: %+v)", edits)
	}
}
