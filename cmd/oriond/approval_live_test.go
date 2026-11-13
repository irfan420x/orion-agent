package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/tools"
)

// --- Live-shape approval repro (rc9 hotfix) ----------------------------
//
// The rc8 scratch proof (TestScratchProofGateAndIsolation) passes, but
// its fake Bot API returns getUpdates INSTANTLY and ignores the offset:
// every poll drains the queue at once. The live daemon long-polls
// Telegram (30s) and the approval tap arrives in a LATER poll cycle,
// minutes after the card, while a long poll is in flight. This test
// reproduces the LIVE shape end to end:
//
//   - real PollStream + real turn handler + real gate + real callback
//     handler (messaging.HandleApprovalCallback), against a fake Bot API that
//     mimics Telegram's long-poll: getUpdates BLOCKS until an update
//     is enqueued or the requested timeout elapses, and honours the
//     offset (only updates with update_id >= offset are returned);
//   - a real turn blocked on approval (scripted provider asks for one
//     shell_exec);
//   - the approval tap delivered as a callback-query update ~12s
//     after the card, i.e. mid-long-poll and in a later poll cycle,
//     well inside the gate window.
//
// If the turn completes with "done", the live delivery path works.
// If the gate times out, the instrumentation log (every getUpdates
// call with offset/timeout/returned IDs, every callback answer,
// periodic gate pending counts) shows exactly where the tap got
// stuck.

// longPollFake is a fake Bot API with Telegram-faithful long-poll
// semantics: getUpdates waits for a queued update (update_id >=
// offset) or the requested timeout, whichever comes first.
type longPollFake struct {
	mu       sync.Mutex
	queue    []messaging.Update // sorted by UpdateID, ascending
	sent     []rc8Sent
	answered []string
	edits    []string
	nextID   int64
	polls    []string // one line per getUpdates call, for diagnosis
}

func newLongPollFake() *longPollFake {
	return &longPollFake{}
}

func (f *longPollFake) enqueue(u messaging.Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, u)
}

func (f *longPollFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	if strings.HasSuffix(r.URL.Path, "/getUpdates") {
		// Long-poll: wait for an update >= offset, or the timeout.
		q := r.URL.Query()
		offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		timeout, _ := strconv.Atoi(q.Get("timeout"))
		if timeout <= 0 {
			timeout = 30
		}
		deadline := time.Now().Add(time.Duration(timeout) * time.Second)
		f.mu.Lock()
		var out []messaging.Update
	poll:
		for {
			out = nil
			for _, u := range f.queue {
				if u.UpdateID >= offset {
					out = append(out, u)
				}
			}
			if len(out) > 0 {
				break
			}
			// Wait until an update arrives or the deadline passes.
			// sync.Cond has no timed wait, so poll the condition
			// on a short ticker (50ms) — cheap, and the wait is
			// still a real block from the caller's view. The wait
			// also aborts when the client goes away (test
			// teardown cancels the poll): without this,
			// httptest.Server.Close blocks until the 30s deadline
			// and every live-shape test pays ~30s on exit.
			f.mu.Unlock()
			select {
			case <-r.Context().Done():
				f.mu.Lock()
				out = nil
				break poll
			case <-time.After(50 * time.Millisecond):
			}
			f.mu.Lock()
			if time.Now().After(deadline) {
				break
			}
		}
		// Consume what we return, so a later poll never replays it.
		if len(out) > 0 {
			keep := f.queue[:0]
			maxOut := out[len(out)-1].UpdateID
			for _, u := range f.queue {
				if u.UpdateID > maxOut {
					keep = append(keep, u)
				}
			}
			f.queue = keep
		}
		ids := make([]int64, len(out))
		for i, u := range out {
			ids[i] = u.UpdateID
		}
		f.polls = append(f.polls, fmt.Sprintf("getUpdates offset=%d timeout=%ds -> %v (waited %s)", offset, timeout, ids, time.Since(deadline.Add(-time.Duration(timeout)*time.Second)).Round(time.Millisecond)))
		data, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		f.mu.Unlock()
		w.Write(data)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		var req struct {
			ChatID      int64           `json:"chat_id"`
			Text        string          `json:"text"`
			ReplyMarkup json.RawMessage `json:"reply_markup"`
		}
		_ = json.Unmarshal(body, &req)
		f.nextID++
		f.sent = append(f.sent, rc8Sent{ChatID: req.ChatID, Text: req.Text, Keyboard: string(req.ReplyMarkup), MessageID: f.nextID})
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, f.nextID)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		f.edits = append(f.edits, string(body))
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
		var req struct {
			CallbackQueryID string `json:"callback_query_id"`
			Text            string `json:"text"`
		}
		_ = json.Unmarshal(body, &req)
		f.answered = append(f.answered, req.CallbackQueryID+" text="+req.Text)
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"orion_proof_bot","first_name":"Orion"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"description":"not found"}`)
	}
}

// TestApprovalCallbackLiveShape is the regression test for the live
// rc8 bug: Irfan tapped Approve on a real approval card and the gate
// timed out (07:58:08 log) with zero callback lines in the daemon
// log. It runs the live shape — long-polling fake, tap in a later
// poll cycle — and fails if the turn does not complete approved.
func TestApprovalCallbackLiveShape(t *testing.T) {
	dir := t.TempDir()
	fake := newLongPollFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 30
	mem, err := memory.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	prov := &rc8ScriptedProvider{}
	agent, err := core.NewAgent(cfg, prov, reg, mem)
	if err != nil {
		t.Fatal(err)
	}
	a := &app.App{Config: cfg, Provider: prov, Tools: reg, Memory: mem, Agent: agent}
	defer a.Close()
	store := messaging.NewSessionStore(filepath.Join(dir, "sessions.json"), true)

	// Live shape: Irfan's own chat — he is both the requester and
	// the owner (first allowlisted chat).
	ownerID := int64(7429947930)
	gw, err := messaging.NewTelegramGateway("123456:AAAAAAAAAAAAAAAAAAAA",
		messaging.WithBaseURL(srv.URL),
		messaging.WithAllowedChats(ownerID),
		messaging.WithLogger(func(format string, args ...any) {
			t.Logf("gateway: "+format, args...)
		}))
	if err != nil {
		t.Fatal(err)
	}
	// The REAL daemon wiring (prompter + expiry hook + card
	// tracker), not a copy of it.
	gate, cards, err := buildApprovalGate(a, gw, ownerID)
	if err != nil {
		t.Fatalf("buildApprovalGate: %v", err)
	}
	// Short window so a failure is fast to observe (live is 90s);
	// the tap below still lands well inside it.
	gate.SetTimeout(25 * time.Second)
	a.SetPermissionGate(gate)
	gw.SetCallbackHandler(func(ctx context.Context, q messaging.TGCallbackQuery) error {
		t.Logf("callback handler invoked: id=%s data=%q from=%v", q.ID, q.Data, q.From)
		err := messaging.HandleApprovalCallback(ctx, gw, gate, ownerID, q, cards, "oriond")
		t.Logf("callback handler returned err=%v pending=%d", err, gate.PendingCount())
		return err
	})

	allowed := map[int64]bool{ownerID: true}
	restart := func() {}
	retry := func(ctx context.Context, chatID int64) (string, *core.TurnStats, error) {
		return "", nil, fmt.Errorf("nothing to retry")
	}
	env := buildCommandEnv(a, store, "orion_proof_bot", allowed, ownerID, restart, retry)
	registry := messaging.DefaultRegistry(env)
	turnFn := buildTurnHandler(a, store, registry, env, ownerID, "orion_proof_bot")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pollErr := make(chan error, 1)
	go func() { pollErr <- gw.PollStream(ctx, turnFn) }()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			fake.mu.Lock()
			ok := cond()
			fake.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	// The message that starts the turn (update 1, live-style
	// sequential IDs — not UnixNano).
	fake.enqueue(messaging.Update{UpdateID: 1, Message: &messaging.TGMessage{
		MessageID: 1, Chat: messaging.TGChat{ID: ownerID}, Text: "run the shell",
		From: &messaging.TGUser{ID: ownerID},
	}})

	// Wait for the approval card, then tap Approve ~12s later —
	// mid-long-poll, in a later poll cycle, inside the window
	// (live: card 15:56, tap ~15:57, window 90s).
	var requestID string
	waitFor("approval prompt", func() bool {
		for _, s := range fake.sent {
			if s.ChatID != ownerID || s.Keyboard == "" {
				continue
			}
			var kb messaging.InlineKeyboardMarkup
			if err := json.Unmarshal([]byte(s.Keyboard), &kb); err != nil {
				continue
			}
			for _, row := range kb.InlineKeyboard {
				for _, b := range row {
					if dec, id, ok := messaging.ParsePermissionCallback(b.CallbackData); ok && dec == "approve" {
						requestID = id
						return true
					}
				}
			}
		}
		return false
	})
	t.Logf("approval card seen, requestID=%s; waiting 12s (tap lands mid-long-poll)...", requestID)
	time.Sleep(12 * time.Second)
	t.Logf("gate pending before tap: %d (want 1)", gate.PendingCount())
	fake.enqueue(messaging.Update{UpdateID: 2, CallbackQuery: &messaging.TGCallbackQuery{
		ID:      "cb-approve-" + requestID,
		From:    &messaging.TGUser{ID: ownerID},
		Message: &messaging.TGMessage{MessageID: 2, Chat: messaging.TGChat{ID: ownerID}},
		Data:    "perm:approve:" + requestID,
	}})
	t.Logf("callback update enqueued at %s", time.Now().Format("15:04:05.000"))

	// The turn must complete approved ("done") well before the
	// 25s gate window ends.
	replied := false
	waitFor("approved reply", func() bool {
		for _, s := range fake.sent {
			if s.ChatID == ownerID && s.Keyboard == "" && strings.HasPrefix(s.Text, "done") {
				replied = true
				return true
			}
		}
		return false
	})
	if !replied {
		t.Fatal("turn did not complete approved")
	}
	t.Logf("turn completed approved; gateway offset=%d", gw.Offset())

	cancel()
	select {
	case err := <-pollErr:
		if err != nil {
			t.Fatalf("PollStream after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PollStream did not return after cancel")
	}

	fake.mu.Lock()
	polls := append([]string(nil), fake.polls...)
	answered := append([]string(nil), fake.answered...)
	fake.mu.Unlock()
	t.Logf("poll trace:\n%s", strings.Join(polls, "\n"))
	t.Logf("callback answers: %v", answered)
	if len(answered) != 1 || !strings.Contains(answered[0], "cb-approve-") {
		t.Errorf("callback queries answered = %v, want exactly the approve tap answered", answered)
	}
}

// TestApprovalCallbackWhileTurnSlotsFull is the rc11 W7 live-shape
// repro of the rc10 incident (2026-11-13 18:30 SGT): Irfan tapped
// Approve on card 253 in-window and the gate timed out with zero tap
// lines. TestApprovalCallbackLiveShape above passes because it never
// fills the turn semaphore — one turn, one slot. The live daemon can
// have all 8 turn slots held (long-running turns, queued turns behind
// the per-chat lock), and on the old code the poll loop acquired the
// callback's slot from that same semaphore, so the tap was never
// dispatched: silent, no log line, gate times out.
//
// This test floods the semaphore with 8 scripted approval turns
// (turn 1 runs and blocks on its gate; turns 2-8 queue behind the
// chat lock, each holding a slot) and then taps turn 1's Approve
// button. It FAILS on the old code (no "done" reply within the wait:
// the tap never reaches Resolve) and PASSES once callbacks use their
// own semaphore.
func TestApprovalCallbackWhileTurnSlotsFull(t *testing.T) {
	dir := t.TempDir()
	fake := newLongPollFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 30
	mem, err := memory.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	prov := &rc8ScriptedProvider{}
	agent, err := core.NewAgent(cfg, prov, reg, mem)
	if err != nil {
		t.Fatal(err)
	}
	a := &app.App{Config: cfg, Provider: prov, Tools: reg, Memory: mem, Agent: agent}
	defer a.Close()
	store := messaging.NewSessionStore(filepath.Join(dir, "sessions.json"), true)

	ownerID := int64(7429947930)
	gw, err := messaging.NewTelegramGateway("123456:AAAAAAAAAAAAAAAAAAAA",
		messaging.WithBaseURL(srv.URL),
		messaging.WithAllowedChats(ownerID),
		messaging.WithLogger(func(format string, args ...any) {
			t.Logf("gateway: "+format, args...)
		}))
	if err != nil {
		t.Fatal(err)
	}
	gate, cards, err := buildApprovalGate(a, gw, ownerID)
	if err != nil {
		t.Fatalf("buildApprovalGate: %v", err)
	}
	gate.SetTimeout(25 * time.Second)
	a.SetPermissionGate(gate)
	gw.SetCallbackHandler(func(ctx context.Context, q messaging.TGCallbackQuery) error {
		t.Logf("callback handler invoked: id=%s data=%q from=%v", q.ID, q.Data, q.From)
		err := messaging.HandleApprovalCallback(ctx, gw, gate, ownerID, q, cards, "oriond")
		t.Logf("callback handler returned err=%v pending=%d", err, gate.PendingCount())
		return err
	})

	allowed := map[int64]bool{ownerID: true}
	restart := func() {}
	retry := func(ctx context.Context, chatID int64) (string, *core.TurnStats, error) {
		return "", nil, fmt.Errorf("nothing to retry")
	}
	env := buildCommandEnv(a, store, "orion_proof_bot", allowed, ownerID, restart, retry)
	registry := messaging.DefaultRegistry(env)
	turnFn := buildTurnHandler(a, store, registry, env, ownerID, "orion_proof_bot")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pollErr := make(chan error, 1)
	go func() { pollErr <- gw.PollStream(ctx, turnFn) }()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			fake.mu.Lock()
			ok := cond()
			fake.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	// Flood the turn semaphore: 8 messages for the same chat. Turn 1
	// runs and blocks on its approval gate; turns 2-8 queue behind
	// the per-chat lock, each holding one of the 8 turn slots.
	for i := int64(1); i <= 8; i++ {
		fake.enqueue(messaging.Update{UpdateID: i, Message: &messaging.TGMessage{
			MessageID: i, Chat: messaging.TGChat{ID: ownerID}, Text: "run the shell",
			From: &messaging.TGUser{ID: ownerID},
		}})
	}

	// Wait for turn 1's approval card, then give the poll loop a
	// moment to dispatch the other 7 messages (microseconds of work,
	// 2s of margin) so all 8 turn slots are held before the tap.
	var requestID string
	waitFor("approval prompt", func() bool {
		for _, s := range fake.sent {
			if s.ChatID != ownerID || s.Keyboard == "" {
				continue
			}
			var kb messaging.InlineKeyboardMarkup
			if err := json.Unmarshal([]byte(s.Keyboard), &kb); err != nil {
				continue
			}
			for _, row := range kb.InlineKeyboard {
				for _, b := range row {
					if dec, id, ok := messaging.ParsePermissionCallback(b.CallbackData); ok && dec == "approve" {
						requestID = id
						return true
					}
				}
			}
		}
		return false
	})
	t.Logf("approval card seen, requestID=%s; all 8 turn slots should now be held; tapping Approve...", requestID)
	time.Sleep(2 * time.Second)
	fake.enqueue(messaging.Update{UpdateID: 9, CallbackQuery: &messaging.TGCallbackQuery{
		ID:      "cb-approve-" + requestID,
		From:    &messaging.TGUser{ID: ownerID},
		Message: &messaging.TGMessage{MessageID: 2, Chat: messaging.TGChat{ID: ownerID}},
		Data:    "perm:approve:" + requestID,
	}})
	t.Logf("callback update enqueued at %s", time.Now().Format("15:04:05.000"))

	// The tap must resolve turn 1's approval and the turn must
	// complete with "done" well before the 25s gate window ends —
	// even though every turn slot is held.
	replied := false
	waitFor("approved reply", func() bool {
		for _, s := range fake.sent {
			if s.ChatID == ownerID && s.Keyboard == "" && strings.HasPrefix(s.Text, "done") {
				replied = true
				return true
			}
		}
		return false
	})
	if !replied {
		t.Fatal("turn did not complete approved while turn slots were full")
	}
	t.Logf("turn completed approved with all turn slots held; gateway offset=%d", gw.Offset())

	cancel()
	select {
	case err := <-pollErr:
		if err != nil {
			t.Fatalf("PollStream after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PollStream did not return after cancel")
	}

	// Drain the queued turns (2-8) before returning: they are
	// denied on cancellation and complete shortly after, but
	// PollStream returns promptly without waiting for them
	// (handlers are left to finish). Returning while they still
	// run lets them create files under the test's TempDir after
	// cleanup starts, failing the test with "directory not
	// empty" (flaky under -race + full-suite load; seen in the
	// rc13 release-check run). The offset is a completion
	// watermark (F-A8-2): it reaches 10 only once every handler
	// for updates 1-9 has finished.
	waitFor("all turn handlers drained", func() bool {
		return gw.Offset() == 10
	})

	fake.mu.Lock()
	answered := append([]string(nil), fake.answered...)
	fake.mu.Unlock()
	t.Logf("callback answers: %v", answered)
	if len(answered) != 1 || !strings.Contains(answered[0], "cb-approve-") {
		t.Errorf("callback queries answered = %v, want exactly the approve tap answered", answered)
	}
}

// card must be edited in place to say it expired AND lose its
// keyboard — a dead card with live buttons looks exactly like a
// broken button, and a tap on it (answered only with a transient
// "expired" toast) is indistinguishable from the gate being broken.
// Same live-shape harness as TestApprovalCallbackLiveShape, but no
// tap is ever delivered.
func TestApprovalCardExpiresOnTimeout(t *testing.T) {
	dir := t.TempDir()
	fake := newLongPollFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 30
	mem, err := memory.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	prov := &rc8ScriptedProvider{}
	agent, err := core.NewAgent(cfg, prov, reg, mem)
	if err != nil {
		t.Fatal(err)
	}
	a := &app.App{Config: cfg, Provider: prov, Tools: reg, Memory: mem, Agent: agent}
	defer a.Close()
	store := messaging.NewSessionStore(filepath.Join(dir, "sessions.json"), true)

	ownerID := int64(7429947930)
	gw, err := messaging.NewTelegramGateway("123456:AAAAAAAAAAAAAAAAAAAA",
		messaging.WithBaseURL(srv.URL),
		messaging.WithAllowedChats(ownerID))
	if err != nil {
		t.Fatal(err)
	}
	gate, cards, err := buildApprovalGate(a, gw, ownerID)
	if err != nil {
		t.Fatalf("buildApprovalGate: %v", err)
	}
	// Short window (live is 90s) so the test runs in ~12s.
	gate.SetTimeout(8 * time.Second)
	a.SetPermissionGate(gate)
	gw.SetCallbackHandler(func(ctx context.Context, q messaging.TGCallbackQuery) error {
		return messaging.HandleApprovalCallback(ctx, gw, gate, ownerID, q, cards, "oriond")
	})

	allowed := map[int64]bool{ownerID: true}
	restart := func() {}
	retry := func(ctx context.Context, chatID int64) (string, *core.TurnStats, error) {
		return "", nil, fmt.Errorf("nothing to retry")
	}
	env := buildCommandEnv(a, store, "orion_proof_bot", allowed, ownerID, restart, retry)
	registry := messaging.DefaultRegistry(env)
	turnFn := buildTurnHandler(a, store, registry, env, ownerID, "orion_proof_bot")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pollErr := make(chan error, 1)
	go func() { pollErr <- gw.PollStream(ctx, turnFn) }()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			fake.mu.Lock()
			ok := cond()
			fake.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	fake.enqueue(messaging.Update{UpdateID: 1, Message: &messaging.TGMessage{
		MessageID: 1, Chat: messaging.TGChat{ID: ownerID}, Text: "run the shell",
		From: &messaging.TGUser{ID: ownerID},
	}})

	// Wait for the approval card and record its message ID.
	var cardMsgID int64
	waitFor("approval prompt", func() bool {
		for _, s := range fake.sent {
			if s.ChatID == ownerID && s.Keyboard != "" {
				cardMsgID = s.MessageID
				return true
			}
		}
		return false
	})
	t.Logf("approval card sent as message %d; nobody taps; waiting for the expiry edit...", cardMsgID)

	// The expiry edit must land on the CARD's message, say Expired,
	// and remove the keyboard — within a few seconds of the 8s
	// window closing.
	expired := false
	waitFor("expiry edit on the card", func() bool {
		for _, e := range fake.edits {
			var body struct {
				MessageID   int64           `json:"message_id"`
				Text        string          `json:"text"`
				ReplyMarkup json.RawMessage `json:"reply_markup"`
			}
			if err := json.Unmarshal([]byte(e), &body); err != nil {
				continue
			}
			if body.MessageID != cardMsgID {
				continue
			}
			if strings.Contains(body.Text, "Expired") && strings.Contains(string(body.ReplyMarkup), `"inline_keyboard":[]`) {
				expired = true
				return true
			}
		}
		return false
	})
	if !expired {
		t.Fatal("the approval card was never expired in place")
	}
	t.Logf("card %d expired in place (text + keyboard removed)", cardMsgID)

	// The turn still completes (the model is told the call timed
	// out and answers), and nobody tapped anything.
	waitFor("reply after timeout", func() bool {
		for _, s := range fake.sent {
			if s.ChatID == ownerID && s.Keyboard == "" && strings.HasPrefix(s.Text, "done") {
				return true
			}
		}
		return false
	})
	if gate.PendingCount() != 0 {
		t.Errorf("pending = %d after the timeout, want 0", gate.PendingCount())
	}

	cancel()
	select {
	case err := <-pollErr:
		if err != nil {
			t.Fatalf("PollStream after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PollStream did not return after cancel")
	}

	fake.mu.Lock()
	answered := append([]string(nil), fake.answered...)
	edits := append([]string(nil), fake.edits...)
	fake.mu.Unlock()
	if len(answered) != 0 {
		t.Errorf("callback queries answered = %v, want none (nobody tapped)", answered)
	}
	t.Logf("edits: %v", edits)
}
