// ask_user_question transport tests (internal/messaging): the
// question registry, the Telegram asker, the installed callback
// dispatch, and the free-text interception in PollStream.
//
// The end-to-end tests prove the two properties the design rests
// on: (1) a tap on a question button reaches the INSTALLED
// callback handler (not a direct call) and resolves the Ask, and
// (2) a typed reply resolves a question asked from INSIDE a turn
// without deadlocking — the reply is intercepted before the
// per-chat lock the asking turn holds.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/permissions"
)

// turnCall is one call the recording turn handler saw.
type turnCall struct {
	chatID int64
	text   string
}

// questionHarness is a live PollStream over a queueFake with the
// question wiring installed exactly as the binaries install it
// (SetQuestionRegistry + InstallApprovalCallbackHandler with a
// real gate and real questions registry). The turn handler is a
// dispatch field: tests replace h.handler before enqueueing
// updates; the default records the call and returns "ok".
type questionHarness struct {
	t         *testing.T
	fake      *queueFake
	gw        *TelegramGateway
	questions *QuestionRegistry
	asker     *TelegramQuestionAsker
	gate      *permissions.Gate
	mu        sync.Mutex
	handler   TurnFunc
	turns     []turnCall
	cancel    context.CancelFunc
	errCh     chan error
}

func newQuestionHarness(t *testing.T) *questionHarness {
	t.Helper()
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77, 78))
	if err != nil {
		t.Fatal(err)
	}
	cards := NewApprovalCards()
	gate := permissions.NewGate(permissions.NewRuleStore(""), ApprovalPrompter(gw, cards, "test"))
	questions := NewQuestionRegistry(77)
	gw.SetQuestionRegistry(questions)
	InstallApprovalExpiry(gate, gw, cards, "test")
	InstallApprovalCallbackHandler(gw, gate, 77, cards, questions, "test")
	if gw.callbackHandlerFor() == nil {
		t.Fatal("InstallApprovalCallbackHandler did not install a callback handler on the gateway")
	}
	h := &questionHarness{
		t: t, fake: fake, gw: gw, questions: questions,
		asker: NewTelegramQuestionAsker(gw, questions, 77, "test"),
		gate:  gate,
	}
	h.handler = h.recordingTurn
	dispatch := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		h.mu.Lock()
		fn := h.handler
		h.mu.Unlock()
		return fn(ctx, chatID, text, emit)
	}
	h.cancel, h.errCh = runPollStream(t, gw, dispatch)
	t.Cleanup(func() { stopPollStream(t, h.cancel, h.errCh) })
	return h
}

func (h *questionHarness) recordingTurn(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	h.mu.Lock()
	h.turns = append(h.turns, turnCall{chatID, text})
	h.mu.Unlock()
	return "ok", nil, nil
}

// turnsSnapshot returns the turn calls recorded so far.
func (h *questionHarness) turnsSnapshot() []turnCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]turnCall(nil), h.turns...)
}

// questionSent returns the latest sent question message for
// chatID and its keyboard markup, or false when none arrived yet.
func (h *questionHarness) questionSent(chatID int64) (queueSent, InlineKeyboardMarkup, bool) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for i := len(h.fake.sent) - 1; i >= 0; i-- {
		s := h.fake.sent[i]
		if s.ChatID != chatID || !strings.HasPrefix(s.Text, "❓") {
			continue
		}
		var kb InlineKeyboardMarkup
		if s.Keyboard != "" {
			if err := json.Unmarshal([]byte(s.Keyboard), &kb); err != nil {
				return s, kb, false
			}
		}
		return s, kb, true
	}
	return queueSent{}, InlineKeyboardMarkup{}, false
}

// questionIDFromKeyboard extracts the question ID from the first
// button of a question keyboard.
func questionIDFromKeyboard(t *testing.T, kb InlineKeyboardMarkup) string {
	t.Helper()
	if len(kb.InlineKeyboard) == 0 || len(kb.InlineKeyboard[0]) == 0 {
		t.Fatal("question keyboard has no buttons")
	}
	id, _, ok := ParseQuestionCallback(kb.InlineKeyboard[0][0].CallbackData)
	if !ok {
		t.Fatalf("first button data %q is not an ask: callback", kb.InlineKeyboard[0][0].CallbackData)
	}
	return id
}

// answered reports whether the fake recorded an answerCallbackQuery
// for callback query id.
func (h *questionHarness) answered(id string) bool {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, a := range h.fake.answered {
		if a == id {
			return true
		}
	}
	return false
}

// editFor returns the edit recorded for message msgID, if any.
func (h *questionHarness) editFor(msgID int64) (queueEdit, bool) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, e := range h.fake.edits {
		if e.MsgID == msgID {
			return e, true
		}
	}
	return queueEdit{}, false
}

// askResult is one completed Ask.
type askResult struct {
	answer string
	source string
	index  int
	err    error
}

// askInBackground starts an Ask in a goroutine and returns its
// result channel.
func (h *questionHarness) askInBackground(ctx context.Context, chatID, senderID int64, question string, options []string, timeout time.Duration) chan askResult {
	ch := make(chan askResult, 1)
	go func() {
		answer, source, index, err := h.asker.Ask(ctx, chatID, senderID, question, options, timeout)
		ch <- askResult{answer, source, index, err}
	}()
	return ch
}

// awaitAsk waits for an Ask result or fails the test.
func awaitAsk(t *testing.T, ch chan askResult) askResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("Ask did not return within 5s (deadlock or lost answer)")
		return askResult{}
	}
}

func TestParseQuestionCallback(t *testing.T) {
	id, idx, ok := ParseQuestionCallback("ask:0123456789abcdef:1")
	if !ok || id != "0123456789abcdef" || idx != 1 {
		t.Errorf("valid callback: got (%q,%d,%v), want (0123456789abcdef,1,true)", id, idx, ok)
	}
	if _, _, ok := ParseQuestionCallback("perm:approve:0123456789abcdef"); ok {
		t.Error("approval callback parsed as a question callback")
	}
	for _, bad := range []string{
		"", "ask:", "ask:xyz", "ask:xyz:abc", "ask:xyz:-1",
		"ask:xyz:1:2", "ask::1", "ASK:xyz:1",
	} {
		if _, _, ok := ParseQuestionCallback(bad); ok {
			t.Errorf("ParseQuestionCallback(%q) = ok, want not-ok", bad)
		}
	}
}

func TestQuestionKeyboard(t *testing.T) {
	kb := QuestionKeyboard("0123456789abcdef", []string{"A", "B"})
	if len(kb.InlineKeyboard) != 2 {
		t.Fatalf("keyboard has %d rows, want 2 (one button per row)", len(kb.InlineKeyboard))
	}
	for i, want := range []string{"A", "B"} {
		row := kb.InlineKeyboard[i]
		if len(row) != 1 || row[0].Text != want {
			t.Errorf("row %d = %v, want one button %q", i, row, want)
		}
		wantData := "ask:0123456789abcdef:" + string(rune('0'+i))
		if row[0].CallbackData != wantData {
			t.Errorf("row %d callback data = %q, want %q", i, row[0].CallbackData, wantData)
		}
	}
	if kb := QuestionKeyboard("0123456789abcdef", nil); len(kb.InlineKeyboard) != 0 {
		t.Errorf("no-options keyboard has %d rows, want 0", len(kb.InlineKeyboard))
	}
}

func TestQuestionRegistrySettle(t *testing.T) {
	// Owner 99, requester 77: the owner may answer even though
	// they did not ask, a stranger may not.
	reg := NewQuestionRegistry(99)
	ch := make(chan questionResult, 1)
	p := &pendingQuestion{id: "0123456789abcdef", chatID: 77, senderID: 77, ch: ch}
	reg.register(p)
	if reg.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1", reg.PendingCount())
	}
	if _, ok := reg.PendingForChat(77); !ok {
		t.Error("PendingForChat(77) found nothing for the pending question")
	}
	if _, ok := reg.PendingForChat(78); ok {
		t.Error("PendingForChat(78) found a question asked in chat 77")
	}
	got, ok := reg.lookup("0123456789abcdef")
	if !ok || got != p {
		t.Fatal("lookup did not return the registered question")
	}
	if !reg.canAnswer(got, 77) {
		t.Error("requester may not answer their own question")
	}
	if !reg.canAnswer(got, 99) {
		t.Error("owner may not answer a question in an allowlisted chat")
	}
	if reg.canAnswer(got, 55) {
		t.Error("stranger may answer a question")
	}
	if !reg.Resolve("0123456789abcdef", questionResult{answer: "yes", source: "button", optionIndex: 0}) {
		t.Fatal("first Resolve returned false, want true (single winner)")
	}
	if reg.Resolve("0123456789abcdef", questionResult{answer: "again", source: "button", optionIndex: 0}) {
		t.Error("second Resolve returned true, want false (already settled)")
	}
	res := <-ch
	if res.answer != "yes" || res.source != "button" || res.optionIndex != 0 {
		t.Errorf("resolved result = %+v, want {yes button 0}", res)
	}
	if reg.PendingCount() != 0 {
		t.Error("settled question is still pending")
	}
	if reg.Resolve("unknown", questionResult{answer: "x", source: "text", optionIndex: -1}) {
		t.Error("Resolve on an unknown id returned true")
	}
	// Settle (the timeout/cancel branch) removes the question;
	// a late Resolve is refused.
	ch2 := make(chan questionResult, 1)
	p2 := &pendingQuestion{id: "aaaaaaaaaaaaaaaa", chatID: 77, senderID: 77, ch: ch2}
	reg.register(p2)
	if !reg.settle(p2, nil) {
		t.Fatal("first settle returned false, want true")
	}
	if reg.settle(p2, nil) {
		t.Error("second settle returned true, want false (already settled)")
	}
	if reg.PendingCount() != 0 {
		t.Error("settled question is still pending")
	}
	if reg.Resolve("aaaaaaaaaaaaaaaa", questionResult{answer: "late", source: "text", optionIndex: -1}) {
		t.Error("Resolve after settle returned true")
	}
	// Nil registry: every method is a no-op that never panics.
	var nilReg *QuestionRegistry
	nilReg.register(p)
	if nilReg.PendingCount() != 0 || nilReg.Resolve("x", questionResult{answer: "a", source: "text", optionIndex: -1}) {
		t.Error("nil registry is not a no-op")
	}
	if _, ok := nilReg.PendingForChat(77); ok {
		t.Error("nil registry PendingForChat found a question")
	}
}

func TestAskQuestionButtonTap(t *testing.T) {
	h := newQuestionHarness(t)
	resCh := h.askInBackground(context.Background(), 77, 77, "Pick one?", []string{"A", "B"}, 30*time.Second)
	var msg queueSent
	var kb InlineKeyboardMarkup
	waitFor(t, "question message for chat 77", func() bool {
		var ok bool
		msg, kb, ok = h.questionSent(77)
		return ok
	})
	if len(kb.InlineKeyboard) != 2 {
		t.Fatalf("question keyboard has %d rows, want 2", len(kb.InlineKeyboard))
	}
	id := questionIDFromKeyboard(t, kb)
	if got := kb.InlineKeyboard[1][0].CallbackData; got != "ask:"+id+":1" {
		t.Errorf("second button callback data = %q, want ask:%s:1", got, id)
	}
	// The tap: option 1 ("B") from the asker, through the
	// INSTALLED handler via PollStream.
	h.fake.enqueue(Update{UpdateID: 1, CallbackQuery: &TGCallbackQuery{
		ID: "cb-tap-1", From: &TGUser{ID: 77},
		Message: &TGMessage{MessageID: msg.MsgID, Chat: TGChat{ID: 77}},
		Data:    "ask:" + id + ":1",
	}})
	res := awaitAsk(t, resCh)
	if res.err != nil {
		t.Fatalf("Ask returned error: %v", res.err)
	}
	if res.answer != "B" || res.source != "button" || res.index != 1 {
		t.Errorf("Ask result = %+v, want {B button 1}", res)
	}
	// The tap was answered (no silent swallow) and the question
	// was edited in place to show the answer, keyboard removed.
	waitFor(t, "tap answer", func() bool { return h.answered("cb-tap-1") })
	edit, ok := h.editFor(msg.MsgID)
	if !ok {
		t.Fatal("question message was not edited in place after the tap")
	}
	if !strings.Contains(edit.Text, "✅ B") || !edit.HasKeys {
		t.Errorf("outcome edit = %+v, want it to show ✅ B with a (now empty) keyboard", edit)
	}
	if h.questions.PendingCount() != 0 {
		t.Error("answered question is still pending")
	}
	if turns := h.turnsSnapshot(); len(turns) != 0 {
		t.Errorf("a turn ran for the tap (%v): a callback must never start a turn", turns)
	}
	// Regression: the same installed handler still routes
	// approval taps to the gate (an unknown request is answered
	// "Unknown button", not swallowed by the ask: branch).
	h.fake.enqueue(Update{UpdateID: 2, CallbackQuery: &TGCallbackQuery{
		ID: "cb-perm-1", From: &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 99, Chat: TGChat{ID: 77}},
		Data:    "perm:approve:0123456789abcdef",
	}})
	waitFor(t, "approval tap answer", func() bool { return h.answered("cb-perm-1") })
}

func TestAskQuestionFreeTextReply(t *testing.T) {
	h := newQuestionHarness(t)
	resCh := h.askInBackground(context.Background(), 77, 77, "Why?", nil, 30*time.Second)
	var msg queueSent
	waitFor(t, "free-text question message", func() bool {
		var ok bool
		msg, _, ok = h.questionSent(77)
		return ok
	})
	if msg.Keyboard != "" {
		t.Errorf("free-text question carried a keyboard %q, want none", msg.Keyboard)
	}
	// The typed reply: it resolves the question and must NOT
	// start a turn (the reply is consumed as the answer).
	h.fake.enqueue(Update{UpdateID: 1, Message: &TGMessage{
		MessageID: 1, Chat: TGChat{ID: 77}, Text: "because", From: &TGUser{ID: 77},
	}})
	res := awaitAsk(t, resCh)
	if res.err != nil {
		t.Fatalf("Ask returned error: %v", res.err)
	}
	if res.answer != "because" || res.source != "text" || res.index != -1 {
		t.Errorf("Ask result = %+v, want {because text -1}", res)
	}
	// The outcome edit runs after Resolve (the answer is
	// delivered first, the edit second), so wait for it rather
	// than assuming it landed before Ask returned.
	waitFor(t, "outcome edit", func() bool {
		_, ok := h.editFor(msg.MsgID)
		return ok
	})
	edit, _ := h.editFor(msg.MsgID)
	if !strings.Contains(edit.Text, "✅ because") {
		t.Errorf("outcome edit = %+v, want it to show ✅ because", edit)
	}
	if turns := h.turnsSnapshot(); len(turns) != 0 {
		t.Errorf("a turn ran for the answer message (%v): a reply that answers a question must not start a turn", turns)
	}
}

func TestAskQuestionTurnAsksAndReplyResolves(t *testing.T) {
	// The deadlock test: a turn (which holds chat 77's lock)
	// calls Ask and blocks; a typed reply in the same chat must
	// still resolve it, because tryAnswerQuestion runs BEFORE
	// the per-chat lock in PollStream's handle. On the old code
	// (reply routed through a turn) the reply would wait on the
	// lock until the 30s Ask timeout, and this test would fail
	// at the 5s waitFor.
	h := newQuestionHarness(t)
	h.mu.Lock()
	h.handler = func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		if text != "ask me" {
			return "ok", nil, nil
		}
		sender, _ := SenderIDFromContext(ctx)
		answer, _, _, err := h.asker.Ask(ctx, chatID, sender, "Pick?", []string{"A", "B"}, 30*time.Second)
		if err != nil {
			return "", nil, err
		}
		return "got: " + answer, nil, nil
	}
	h.mu.Unlock()
	h.fake.enqueue(Update{UpdateID: 1, Message: &TGMessage{
		MessageID: 1, Chat: TGChat{ID: 77}, Text: "ask me", From: &TGUser{ID: 77},
	}})
	var msg queueSent
	waitFor(t, "question from inside the turn", func() bool {
		var ok bool
		msg, _, ok = h.questionSent(77)
		return ok
	})
	// The typed reply "A": it answers the question (free text),
	// the turn unblocks, and its reply is sent.
	h.fake.enqueue(Update{UpdateID: 2, Message: &TGMessage{
		MessageID: 2, Chat: TGChat{ID: 77}, Text: "A", From: &TGUser{ID: 77},
	}})
	waitFor(t, "turn reply got: A", func() bool {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		for _, s := range h.fake.sent {
			if strings.Contains(s.Text, "got: A") {
				return true
			}
		}
		return false
	})
	if h.questions.PendingCount() != 0 {
		t.Error("answered question is still pending")
	}
	// The reply itself never started a second turn: the only
	// turn was the "ask me" one (the recording turn is replaced
	// by the asking handler here, so check the question message
	// count instead — exactly one question was asked).
	if n := countSent(h.fake, 77, "❓"); n != 1 {
		t.Errorf("sent %d question messages, want 1", n)
	}
	_ = msg
}

func TestAskQuestionCommandNotConsumed(t *testing.T) {
	h := newQuestionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resCh := h.askInBackground(ctx, 77, 77, "Why?", nil, 30*time.Second)
	waitFor(t, "question message", func() bool {
		_, _, ok := h.questionSent(77)
		return ok
	})
	// A command is NEVER an answer: it flows to a normal turn,
	// and the question stays pending.
	h.fake.enqueue(Update{UpdateID: 1, Message: &TGMessage{
		MessageID: 1, Chat: TGChat{ID: 77}, Text: "/restart", From: &TGUser{ID: 77},
	}})
	waitFor(t, "turn for /restart", func() bool {
		for _, c := range h.turnsSnapshot() {
			if c.chatID == 77 && c.text == "/restart" {
				return true
			}
		}
		return false
	})
	if h.questions.PendingCount() != 1 {
		t.Errorf("PendingCount = %d after /restart, want 1 (the command must not answer the question)", h.questions.PendingCount())
	}
	cancel()
	res := awaitAsk(t, resCh)
	if res.err == nil || !errors.Is(res.err, context.Canceled) {
		t.Errorf("Ask after cancel: error %v, want a cancelled error", res.err)
	}
}

func TestAskQuestionStrangerTapRefused(t *testing.T) {
	h := newQuestionHarness(t)
	resCh := h.askInBackground(context.Background(), 77, 77, "Pick one?", []string{"A", "B"}, 30*time.Second)
	var msg queueSent
	var kb InlineKeyboardMarkup
	waitFor(t, "question message", func() bool {
		var ok bool
		msg, kb, ok = h.questionSent(77)
		return ok
	})
	id := questionIDFromKeyboard(t, kb)
	// A tap from an allowlisted user who is neither the asker
	// nor the owner (78): refused with a toast and the question
	// stays pending. (A tap from a non-allowlisted user never
	// reaches the handler at all — PollStream drops it at the
	// allowlist check, the same as for approvals.)
	h.fake.enqueue(Update{UpdateID: 1, CallbackQuery: &TGCallbackQuery{
		ID: "cb-stranger", From: &TGUser{ID: 78},
		Message: &TGMessage{MessageID: msg.MsgID, Chat: TGChat{ID: 77}},
		Data:    "ask:" + id + ":0",
	}})
	waitFor(t, "stranger tap answer", func() bool { return h.answered("cb-stranger") })
	if h.questions.PendingCount() != 1 {
		t.Error("a non-asker's tap resolved the question")
	}
	select {
	case res := <-resCh:
		t.Fatalf("Ask returned after a non-asker tap: %+v", res)
	case <-time.After(200 * time.Millisecond):
	}
	// The asker can still answer after the refused tap.
	h.fake.enqueue(Update{UpdateID: 2, CallbackQuery: &TGCallbackQuery{
		ID: "cb-owner", From: &TGUser{ID: 77},
		Message: &TGMessage{MessageID: msg.MsgID, Chat: TGChat{ID: 77}},
		Data:    "ask:" + id + ":0",
	}})
	res := awaitAsk(t, resCh)
	if res.err != nil || res.answer != "A" {
		t.Errorf("Ask after the refused tap = %+v, want A", res)
	}
}

func TestAskQuestionTimeout(t *testing.T) {
	h := newQuestionHarness(t)
	resCh := h.askInBackground(context.Background(), 77, 77, "Pick one?", []string{"A", "B"}, 150*time.Millisecond)
	var msg queueSent
	waitFor(t, "question message", func() bool {
		var ok bool
		msg, _, ok = h.questionSent(77)
		return ok
	})
	res := awaitAsk(t, resCh)
	if res.err == nil {
		t.Fatal("Ask with no answer returned no error, want a timeout")
	}
	if !errors.Is(res.err, context.DeadlineExceeded) || !strings.Contains(res.err.Error(), "no answer within") {
		t.Errorf("Ask timeout error = %v, want a DeadlineExceeded saying no answer within the limit", res.err)
	}
	// The question was edited in place as expired, keyboard
	// removed (so a later tap cannot look live).
	waitFor(t, "expiry edit", func() bool {
		_, ok := h.editFor(msg.MsgID)
		return ok
	})
	edit, _ := h.editFor(msg.MsgID)
	if !strings.Contains(edit.Text, "Expired") || !edit.HasKeys {
		t.Errorf("expiry edit = %+v, want Expired with a (now empty) keyboard", edit)
	}
	if h.questions.PendingCount() != 0 {
		t.Error("timed-out question is still pending")
	}
}

func TestAskQuestionContextCancel(t *testing.T) {
	h := newQuestionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	resCh := h.askInBackground(ctx, 77, 77, "Pick one?", []string{"A", "B"}, 30*time.Second)
	var msg queueSent
	waitFor(t, "question message", func() bool {
		var ok bool
		msg, _, ok = h.questionSent(77)
		return ok
	})
	cancel()
	res := awaitAsk(t, resCh)
	if res.err == nil || !errors.Is(res.err, context.Canceled) {
		t.Errorf("Ask after cancel: error %v, want a cancelled error", res.err)
	}
	if h.questions.PendingCount() != 0 {
		t.Error("cancelled question is still pending")
	}
	// No expiry edit on cancel: the question was not left
	// hanging, it was cancelled.
	time.Sleep(100 * time.Millisecond)
	if edit, ok := h.editFor(msg.MsgID); ok && strings.Contains(edit.Text, "Expired") {
		t.Errorf("cancelled question got an expiry edit: %+v", edit)
	}
}

func TestAskQuestionPerChatIsolation(t *testing.T) {
	h := newQuestionHarness(t)
	resCh := h.askInBackground(context.Background(), 77, 77, "Why?", nil, 30*time.Second)
	waitFor(t, "question message for chat 77", func() bool {
		_, _, ok := h.questionSent(77)
		return ok
	})
	// A message in chat 78 is NOT an answer to chat 77's
	// question: it flows to a normal turn in chat 78.
	h.fake.enqueue(Update{UpdateID: 1, Message: &TGMessage{
		MessageID: 1, Chat: TGChat{ID: 78}, Text: "answer from 78", From: &TGUser{ID: 78},
	}})
	waitFor(t, "turn for chat 78", func() bool {
		for _, c := range h.turnsSnapshot() {
			if c.chatID == 78 && c.text == "answer from 78" {
				return true
			}
		}
		return false
	})
	if h.questions.PendingCount() != 1 {
		t.Error("a message in chat 78 answered chat 77's question")
	}
	// The answer from chat 77 resolves it.
	h.fake.enqueue(Update{UpdateID: 2, Message: &TGMessage{
		MessageID: 2, Chat: TGChat{ID: 77}, Text: "answer from 77", From: &TGUser{ID: 77},
	}})
	res := awaitAsk(t, resCh)
	if res.err != nil || res.answer != "answer from 77" || res.source != "text" {
		t.Errorf("Ask result = %+v, want {answer from 77 text -1}", res)
	}
}

func TestAskQuestionSendFailure(t *testing.T) {
	// A gateway whose Bot API is unreachable: Ask must fail
	// cleanly and leave no pending question behind.
	gw, err := NewTelegramGateway(testToken, WithBaseURL("http://127.0.0.1:1"), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	questions := NewQuestionRegistry(77)
	asker := NewTelegramQuestionAsker(gw, questions, 77, "test")
	_, _, _, err = asker.Ask(context.Background(), 77, 77, "Pick?", []string{"A"}, 30*time.Second)
	if err == nil {
		t.Fatal("Ask with an unreachable Bot API returned no error")
	}
	if questions.PendingCount() != 0 {
		t.Error("failed Ask left a pending question behind")
	}
}

func TestHandleQuestionCallbackNilRegistry(t *testing.T) {
	// The test call sites install the handler with nil questions
	// (approval-only wiring): an ask: tap must be answered as
	// expired, never panic, never resolve anything.
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	q := TGCallbackQuery{
		ID: "cb-nil", From: &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}},
		Data:    "ask:0123456789abcdef:0",
	}
	if err := HandleQuestionCallback(context.Background(), gw, nil, q, "test"); err != nil {
		t.Fatalf("HandleQuestionCallback with nil registry: %v", err)
	}
	fake.mu.Lock()
	answered := false
	for _, a := range fake.answered {
		if a == "cb-nil" {
			answered = true
		}
	}
	fake.mu.Unlock()
	if !answered {
		t.Error("ask: tap with a nil registry was not answered (silent swallow)")
	}
}

// countSent counts sent messages for chatID whose text contains
// substr.
func countSent(f *queueFake, chatID int64, substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sent {
		if s.ChatID == chatID && strings.Contains(s.Text, substr) {
			n++
		}
	}
	return n
}
