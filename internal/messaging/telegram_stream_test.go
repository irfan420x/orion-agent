package messaging

import (
	"context"
	"encoding/json"
	"errors"
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

// streamFake is a fake Telegram server for PollStream tests. It records
// sendMessage (plain + HTML), editMessageText, sendChatAction and
// setMyCommands calls, and serves scripted getUpdates.
type streamFake struct {
	mu          sync.Mutex
	sent        []streamSent // every sendMessage call, in order
	edits       []streamEdit // every editMessageText call, in order
	actions     []string     // every sendChatAction action, in order
	commands    []BotCommand // last setMyCommands body seen
	answered    []string     // callback_query_ids answered, in order
	keyboards   []string     // raw sendMessage bodies that carried reply_markup
	updates     []Update
	updateCalls int
	// getUpdatesQueries records the allowed_updates query value of
	// every getUpdates call, in order ("" when the call carried no
	// filter) — the F-A16-2 tests assert the filter is sent on the
	// first fetch only.
	getUpdatesQueries []string
	// failGetUpdates makes the first that many getUpdates calls
	// fail with HTTP 500, to prove a failed filter reset neither
	// kills polling nor loses the reset (it is retried on the
	// next fetch). 0 (the default) fails nothing.
	failGetUpdates int
	nextMsgID      int64
	// failFirstEdit makes the first editMessageText call fail with a
	// "message to edit not found" error, to exercise the fallback.
	failFirstEdit bool
	editCalls     int
	// notModified makes editMessageText fail with Telegram's
	// "message is not modified" error.
	notModified bool
}

type streamSent struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
	MsgID     int64  // message_id the fake returned
}

type streamEdit struct {
	ChatID    int64  `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

func (f *streamFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		var msg streamSent
		if err := json.Unmarshal(body, &msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"ok":false,"description":"bad json"}`)
			return
		}
		f.mu.Lock()
		f.nextMsgID++
		msg.MsgID = f.nextMsgID
		f.sent = append(f.sent, msg)
		if strings.Contains(string(body), "reply_markup") {
			f.keyboards = append(f.keyboards, string(body))
		}
		id := msg.MsgID
		f.mu.Unlock()
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, id)
	case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
		var req struct {
			CallbackQueryID string `json:"callback_query_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"ok":false,"description":"bad json"}`)
			return
		}
		f.mu.Lock()
		f.answered = append(f.answered, req.CallbackQueryID)
		f.mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		var e streamEdit
		if err := json.Unmarshal(body, &e); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"ok":false,"description":"bad json"}`)
			return
		}
		f.mu.Lock()
		f.editCalls++
		call := f.editCalls
		failFirst := f.failFirstEdit && call == 1
		notMod := f.notModified
		if !failFirst && !notMod {
			f.edits = append(f.edits, e)
		}
		f.mu.Unlock()
		switch {
		case failFirst:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
		case notMod:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content of the message"}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
		var req struct {
			ChatID int64  `json:"chat_id"`
			Action string `json:"action"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.actions = append(f.actions, req.Action)
		f.mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
		var req struct {
			Commands []BotCommand `json:"commands"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"ok":false,"description":"bad json"}`)
			return
		}
		f.mu.Lock()
		f.commands = req.Commands
		f.mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		f.mu.Lock()
		f.updateCalls++
		call := f.updateCalls
		f.getUpdatesQueries = append(f.getUpdatesQueries, r.URL.Query().Get("allowed_updates"))
		fail := call <= f.failGetUpdates
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"simulated getUpdates failure"}`)
			return
		}
		var out []Update
		if call == f.failGetUpdates+1 {
			out = f.updates
		} else {
			out = []Update{}
		}
		data, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		w.Write(data)
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"orion_test_bot","first_name":"Orion"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"description":"not found"}`)
	}
}

func (f *streamFake) snapshot() (sent []streamSent, edits []streamEdit, actions []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sent = append([]streamSent(nil), f.sent...)
	edits = append([]streamEdit(nil), f.edits...)
	actions = append([]string(nil), f.actions...)
	return sent, edits, actions
}

// getUpdatesQueriesSnapshot returns the recorded allowed_updates
// query values, in call order.
func (f *streamFake) getUpdatesQueriesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.getUpdatesQueries...)
}

// keyboardsSnapshot returns the raw sendMessage bodies that carried
// reply_markup, in order.
func (f *streamFake) keyboardsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keyboards...)
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// runPollStream starts PollStream in a goroutine and returns a cancel
// func plus an error channel for its result.
func runPollStream(t *testing.T, g *TelegramGateway, handler TurnFunc) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- g.PollStream(ctx, handler) }()
	return cancel, errCh
}

func stopPollStream(t *testing.T, cancel context.CancelFunc, errCh chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("PollStream after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PollStream did not return after ctx cancel")
	}
}

// scriptedTurn is the fake end-to-end TurnFunc: shell_exec,
// file_write, the same shell_exec twice, calculator — with finishes.
func scriptedTurn(stats *core.TurnStats) TurnFunc {
	return func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		shell := func(cmd string) {
			args := mustJSONArgsForTest("command", cmd)
			emit(started("shell_exec", args))
			emit(finished("shell_exec", args, nil))
		}
		shell("echo hello")
		fw := mustJSONArgsForTest("path", "/home/irfan/bin/pulse.sh", "content", "x")
		emit(started("file_write", fw))
		emit(finished("file_write", fw, nil))
		shell("chmod +x ~/bin/pulse.sh && ~/bin/pulse.sh")
		shell("chmod +x ~/bin/pulse.sh && ~/bin/pulse.sh")
		calc := mustJSONArgsForTest("expression", "17*23+5")
		emit(started("calculator", calc))
		emit(finished("calculator", calc, nil))
		return "done: 396", stats, nil
	}
}

func mustJSONArgsForTest(pairs ...string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%q", pairs[i], pairs[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

// TestPollStreamCardEndToEnd is the fake end-to-end: a scripted turn
// drives PollStream and the exact final card HTML the fake server
// received is asserted.
func TestPollStreamCardEndToEnd(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	stats := &core.TurnStats{Provider: "deepseek", Model: "deepseek-chat", Tokens: 1240, Steps: 3, Duration: 4200 * time.Millisecond}
	cancel, errCh := runPollStream(t, g, scriptedTurn(stats))
	defer stopPollStream(t, cancel, errCh)

	// Wait for the plain reply (the last thing the turn sends).
	waitFor(t, "final reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.ParseMode == "" && strings.HasPrefix(m.Text, "done: 396") {
				return true
			}
		}
		return false
	})
	// Give the final card edit a moment to land (it precedes the reply,
	// but the reply wait above can win the race on a fast loopback).
	waitFor(t, "final card edit", func() bool {
		_, edits, _ := fake.snapshot()
		return len(edits) >= 1
	})

	sent, edits, actions := fake.snapshot()

	// Typing actions were sent.
	if len(actions) == 0 || actions[0] != "typing" {
		t.Fatalf("chat actions = %v, want at least one %q", actions, "typing")
	}

	// The card was created via sendMessage with parse_mode HTML.
	var cardSends []streamSent
	for _, m := range sent {
		if m.ParseMode == "HTML" {
			cardSends = append(cardSends, m)
		}
	}
	if len(cardSends) == 0 {
		t.Fatal("no card message was sent")
	}

	// The exact final card HTML is the last edit (or the last card send
	// when no edit happened).
	finalCard := ""
	if len(edits) > 0 {
		finalCard = edits[len(edits)-1].Text
	} else {
		finalCard = cardSends[len(cardSends)-1].Text
	}
	want := "💻 <b>terminal</b>\n" +
		"<blockquote><b>Shell</b>  &lt;/&gt;\n<code>echo hello</code></blockquote>\n" +
		"✍️ Writing /home/irfan/bin/pulse.sh\n" +
		"<blockquote><b>Shell</b>  &lt;/&gt;\n<code>chmod +x ~/bin/pulse.sh &amp;&amp; ~/bin/pulse.sh</code></blockquote>\n" +
		"(×2)\n" +
		"🧮 17*23+5"
	if finalCard != want {
		t.Fatalf("final card = %q, want %q", finalCard, want)
	}

	// The footer is appended to the plain reply.
	for _, m := range sent {
		if m.ParseMode == "" && strings.HasPrefix(m.Text, "done: 396") {
			if want := "done: 396\n\n— deepseek-chat · 1,240 tokens · 3 steps · 4.2s"; m.Text != want {
				t.Fatalf("reply = %q, want %q", m.Text, want)
			}
			return
		}
	}
	t.Fatal("final reply not found")
}

// TestPollStreamNoFooterWhenStatsNil verifies no footer is appended
// when the handler returns nil stats.
func TestPollStreamNoFooterWhenStatsNil(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 9}, Text: "hi", From: &TGUser{ID: 9}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "plain reply", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == "plain reply" {
				return true
			}
		}
		return false
	})
	sent, _, _ := fake.snapshot()
	if sent[0].Text != "plain reply" {
		t.Fatalf("reply = %q, want %q (no footer)", sent[0].Text, "plain reply")
	}
	if strings.Contains(sent[0].Text, "—") {
		t.Fatalf("reply = %q, must not contain a footer", sent[0].Text)
	}
}

// TestPollStreamNoToolCallsSendsNoCard verifies a turn with no tool
// calls sends no card message at all.
func TestPollStreamNoToolCallsSendsNoCard(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 9}, Text: "hi", From: &TGUser{ID: 9}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		return "just text", nil, nil
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == "just text" {
				return true
			}
		}
		return false
	})
	sent, edits, _ := fake.snapshot()
	for _, m := range sent {
		if m.ParseMode == "HTML" {
			t.Fatalf("card message sent for a turn with no tool calls: %+v", m)
		}
	}
	if len(edits) != 0 {
		t.Fatalf("edits = %d, want 0 for a turn with no tool calls", len(edits))
	}
}

// TestPollStreamEditFailureFallsBackToFreshCard verifies a failed card
// edit sends a fresh card message and the turn still completes.
func TestPollStreamEditFailureFallsBackToFreshCard(t *testing.T) {
	fake := &streamFake{
		failFirstEdit: true,
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 11}, Text: "hi", From: &TGUser{ID: 11}}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	stats := &core.TurnStats{Provider: "deepseek", Model: "deepseek-chat", Tokens: 10, Steps: 1, Duration: 100 * time.Millisecond}
	cancel, errCh := runPollStream(t, g, scriptedTurn(stats))
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "reply after edit failure", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.ParseMode == "" && strings.HasPrefix(m.Text, "done: 396") {
				return true
			}
		}
		return false
	})
	sent, _, _ := fake.snapshot()
	var cardSends []streamSent
	for _, m := range sent {
		if m.ParseMode == "HTML" {
			cardSends = append(cardSends, m)
		}
	}
	// One card from creation, one fresh card after the edit failed.
	if len(cardSends) < 2 {
		t.Fatalf("card sends = %d, want >= 2 (creation + fallback after edit failure)", len(cardSends))
	}
	if cardSends[0].MsgID == cardSends[len(cardSends)-1].MsgID {
		t.Fatalf("fallback card reused message id %d, want a fresh message", cardSends[0].MsgID)
	}
	// The fallback card carries the full final render.
	last := cardSends[len(cardSends)-1].Text
	if !strings.Contains(last, "🧮 17*23+5") || !strings.Contains(last, "(×2)") {
		t.Fatalf("fallback card = %q, want the full final render", last)
	}
}

// TestPollStreamHandlerError verifies a handler error sends the
// redacted apology and the card gets a final edit showing the failure.
func TestPollStreamHandlerError(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 13}, Text: "boom", From: &TGUser{ID: 13}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	handler := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		args := mustJSONArgsForTest("command", "false")
		emit(started("shell_exec", args))
		emit(finished("shell_exec", args, errors.New("exit status 1")))
		return "", nil, errors.New("kaboom")
	}
	cancel, errCh := runPollStream(t, g, handler)
	defer stopPollStream(t, cancel, errCh)

	waitFor(t, "error reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if strings.Contains(m.Text, "Sorry, I hit an error: kaboom") {
				return true
			}
		}
		return false
	})
	sent, edits, _ := fake.snapshot()
	if len(edits) == 0 {
		t.Fatal("no card edit after handler error; want a final edit showing the failed state")
	}
	if last := edits[len(edits)-1].Text; !strings.Contains(last, " — failed") {
		t.Fatalf("final card edit = %q, want the failed marker", last)
	}
	for _, m := range sent {
		if strings.Contains(m.Text, "Sorry, I hit an error: kaboom") {
			return
		}
	}
	t.Fatal("error apology not sent")
}

// TestPollStreamNilHandler verifies PollStream rejects a nil handler.
func TestPollStreamNilHandler(t *testing.T) {
	g, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if err := g.PollStream(context.Background(), nil); err == nil {
		t.Fatal("PollStream(nil handler): expected error, got nil")
	}
}

// TestSetMyCommands verifies the setMyCommands wire shape.
func TestSetMyCommands(t *testing.T) {
	fake := &streamFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	cmds := []BotCommand{
		{Name: "help", Description: "Show help"},
		{Name: "new", Description: "Start a new conversation"},
	}
	if err := g.SetMyCommands(context.Background(), cmds); err != nil {
		t.Fatalf("SetMyCommands: %v", err)
	}
	fake.mu.Lock()
	got := append([]BotCommand(nil), fake.commands...)
	fake.mu.Unlock()
	if len(got) != 2 || got[0] != cmds[0] || got[1] != cmds[1] {
		t.Fatalf("setMyCommands = %+v, want %+v", got, cmds)
	}
}

// TestSendMessageHTMLAndEdit verifies SendMessageHTML returns the
// message id and EditMessageText edits it; "message is not modified"
// is treated as success.
func TestSendMessageHTMLAndEdit(t *testing.T) {
	fake := &streamFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	id, err := g.SendMessageHTML(context.Background(), 42, "💻 <b>terminal</b>")
	if err != nil {
		t.Fatalf("SendMessageHTML: %v", err)
	}
	if id != 1 {
		t.Fatalf("SendMessageHTML message_id = %d, want 1", id)
	}
	sent, _, _ := fake.snapshot()
	if len(sent) != 1 || sent[0].ParseMode != "HTML" || sent[0].Text != "💻 <b>terminal</b>" {
		t.Fatalf("sendMessage = %+v, want one HTML message", sent)
	}
	if err := g.EditMessageText(context.Background(), 42, id, "💻 <b>terminal</b>\nmore"); err != nil {
		t.Fatalf("EditMessageText: %v", err)
	}

	// "message is not modified" must not fail the turn.
	fake.mu.Lock()
	fake.notModified = true
	fake.mu.Unlock()
	if err := g.EditMessageText(context.Background(), 42, id, "same"); err != nil {
		t.Fatalf("EditMessageText(not modified) = %v, want nil", err)
	}
}

// TestSendChatAction verifies the sendChatAction wire call.
func TestSendChatAction(t *testing.T) {
	fake := &streamFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	if err := g.SendChatAction(context.Background(), 42, "typing"); err != nil {
		t.Fatalf("SendChatAction: %v", err)
	}
	_, _, actions := fake.snapshot()
	if len(actions) != 1 || actions[0] != "typing" {
		t.Fatalf("chat actions = %v, want [typing]", actions)
	}
}

// TestPollWrapperStillStreams verifies Poll still works as a thin
// wrapper over PollStream (no card, no footer).
func TestPollWrapperStillStreams(t *testing.T) {
	fake := &streamFake{updates: []Update{
		{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- g.Poll(ctx, func(ctx context.Context, chatID int64, text string) (string, error) {
			return "pong: " + text, nil
		})
	}()
	waitFor(t, "Poll reply", func() bool {
		sent, _, _ := fake.snapshot()
		for _, m := range sent {
			if m.Text == "pong: hi" {
				return true
			}
		}
		return false
	})
	sent, _, _ := fake.snapshot()
	if sent[0].Text != "pong: hi" {
		t.Fatalf("Poll reply = %q, want %q", sent[0].Text, "pong: hi")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after ctx cancel")
	}
}
