package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// rc8TestApp builds a real App over a scratch HOME with an ollama
// default and an openai entry carrying a fake key (synthetic,
// unusable — NewProvider only stores it; no request is ever made).
// It returns the app, the session store, and the data dir.
func rc8TestApp(t *testing.T) (*app.App, *messaging.SessionStore, string) {
	t.Helper()
	scratchHome(t)
	home := os.Getenv("HOME")
	cfg := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"},"openai":{"name":"openai","api_key":"test-fake-key-for-rc8","model":"gpt-4o-mini"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA","telegram_allowed_chats":[7429947930,77],"data_dir":"` + home + `/.orion"}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := app.Build("")
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	store := messaging.NewSessionStore(filepath.Join(home, ".orion", "sessions.json"), true)
	return a, store, home + "/.orion"
}

// rc8TurnHandler builds the daemon turn handler for tests: owner is
// the first allowlisted chat (7429947930), restart signals on the
// returned channel.
func rc8TurnHandler(t *testing.T, a *app.App, store *messaging.SessionStore) (messaging.TurnFunc, chan struct{}) {
	t.Helper()
	allowed := map[int64]bool{7429947930: true, 77: true}
	restartCh := make(chan struct{}, 1)
	restart := func() { restartCh <- struct{}{} }
	retry := func(ctx context.Context, chatID int64) (string, *core.TurnStats, error) {
		return "", nil, fmt.Errorf("nothing to retry")
	}
	env := buildCommandEnv(a, store, "testbot", allowed, 7429947930, restart, retry)
	registry := messaging.DefaultRegistry(env)
	return buildTurnHandler(a, store, registry, env, 7429947930, "testbot"), restartCh
}

// turnAs runs turnFn as sender in chat and returns its reply.
func turnAs(t *testing.T, fn messaging.TurnFunc, sender, chat int64, text string) (string, error) {
	t.Helper()
	ctx := messaging.WithSenderID(context.Background(), sender)
	reply, _, err := fn(ctx, chat, text, nil)
	return reply, err
}

func TestOwnerRestartRefusedForNonOwner(t *testing.T) {
	a, store, _ := rc8TestApp(t)
	fn, restartCh := rc8TurnHandler(t, a, store)

	reply, err := turnAs(t, fn, 77, 77, "/restart")
	if err != nil {
		t.Fatalf("/restart as non-owner: unexpected error %v", err)
	}
	if !strings.Contains(reply, "owner-only") {
		t.Errorf("/restart reply as non-owner = %q, want an owner-only refusal", reply)
	}
	select {
	case <-restartCh:
		t.Fatal("/restart as non-owner signalled a restart")
	default:
	}

	// The owner restarts fine.
	reply, err = turnAs(t, fn, 7429947930, 7429947930, "/restart")
	if err != nil {
		t.Fatalf("/restart as owner: unexpected error %v", err)
	}
	if !strings.Contains(reply, "Restarting") {
		t.Errorf("/restart reply as owner = %q, want the restarting reply", reply)
	}
	select {
	case <-restartCh:
	case <-time.After(2 * time.Second):
		t.Fatal("/restart as owner did not signal a restart")
	}
}

func TestWhoamiShowsOwnerRole(t *testing.T) {
	a, store, _ := rc8TestApp(t)
	fn, _ := rc8TurnHandler(t, a, store)

	reply, _ := turnAs(t, fn, 7429947930, 7429947930, "/whoami")
	if !strings.Contains(reply, "Role: owner") {
		t.Errorf("owner /whoami = %q, want Role: owner", reply)
	}
	reply, _ = turnAs(t, fn, 77, 77, "/whoami")
	if !strings.Contains(reply, "Role: admin") {
		t.Errorf("non-owner /whoami = %q, want Role: admin", reply)
	}
	// The owner messaging from a group chat is still the owner.
	reply, _ = turnAs(t, fn, 7429947930, -100, "/whoami")
	if !strings.Contains(reply, "Role: owner") {
		t.Errorf("owner-in-group /whoami = %q, want Role: owner", reply)
	}
}

func TestPerChatModelCommandThroughTurnHandler(t *testing.T) {
	a, store, _ := rc8TestApp(t)
	fn, _ := rc8TurnHandler(t, a, store)

	// Chat 77 switches to openai; the reply names the new model.
	reply, err := turnAs(t, fn, 77, 77, "/model openai")
	if err != nil {
		t.Fatalf("/model openai: %v", err)
	}
	if !strings.Contains(reply, "openai") {
		t.Errorf("/model openai reply = %q, want it to name openai", reply)
	}
	// The other chat still reports the daemon default.
	reply, _ = turnAs(t, fn, 7429947930, 7429947930, "/model")
	if !strings.Contains(reply, "Current: ollama / llama3.1") {
		t.Errorf("owner /model after chat 77 switched = %q, want Current: ollama / llama3.1 (unaffected)", reply)
	}
	// The daemon default itself is untouched.
	if got := a.ProviderName(); got != "ollama" {
		t.Errorf("global provider = %q, want ollama", got)
	}
}

func TestPerChatNewCommandThroughTurnHandler(t *testing.T) {
	a, store, dir := rc8TestApp(t)
	fn, _ := rc8TurnHandler(t, a, store)

	// Seed both chats' histories directly in their per-chat files
	// (the agent loop is not needed to prove /new's scope).
	seed := func(chat int64, text string) {
		chatDir := filepath.Join(dir, "chats", fmt.Sprint(chat))
		fs, err := memory.NewFileStore(chatDir)
		if err != nil {
			t.Fatalf("seed FileStore chat %d: %v", chat, err)
		}
		defer fs.Close()
		if err := fs.AddMessage(model.Message{Role: "user", Content: text}); err != nil {
			t.Fatalf("seed AddMessage: %v", err)
		}
	}
	seed(77, "chat 77 secret")
	seed(7429947930, "owner secret")

	reply, err := turnAs(t, fn, 77, 77, "/new")
	if err != nil {
		t.Fatalf("/new: %v", err)
	}
	if !strings.Contains(reply, "new session") {
		t.Errorf("/new reply = %q, want the new-session reply", reply)
	}
	read := func(chat int64) []model.Message {
		chatDir := filepath.Join(dir, "chats", fmt.Sprint(chat))
		fs, err := memory.NewFileStore(chatDir)
		if err != nil {
			t.Fatalf("read FileStore chat %d: %v", chat, err)
		}
		defer fs.Close()
		msgs, err := fs.RecentMessages(0)
		if err != nil {
			t.Fatalf("read RecentMessages: %v", err)
		}
		return msgs
	}
	if msgs := read(77); len(msgs) != 0 {
		t.Errorf("chat 77 history after /new = %d message(s), want 0", len(msgs))
	}
	if msgs := read(7429947930); len(msgs) != 1 || msgs[0].Content != "owner secret" {
		t.Errorf("owner history after chat 77's /new = %v, want the owner's message intact", msgs)
	}
}

func TestPerChatPauseThroughTurnHandler(t *testing.T) {
	a, store, _ := rc8TestApp(t)
	fn, _ := rc8TurnHandler(t, a, store)

	reply, _ := turnAs(t, fn, 77, 77, "/pause")
	if !strings.Contains(reply, "Paused") {
		t.Errorf("/pause reply = %q, want the paused reply", reply)
	}
	// Chat 77 is paused: a normal message gets the paused reply
	// and never reaches the agent.
	reply, err := turnAs(t, fn, 77, 77, "hello?")
	if err != nil {
		t.Fatalf("paused message: unexpected error %v", err)
	}
	if !strings.Contains(reply, "paused") {
		t.Errorf("paused chat reply = %q, want the paused reply", reply)
	}
	// The owner is NOT paused by chat 77's /pause: the message
	// reaches the agent (which fails fast — no Ollama server — but
	// the point is it RAN instead of answering "paused").
	reply, err = turnAs(t, fn, 7429947930, 7429947930, "hello?")
	if err == nil {
		t.Errorf("owner message: expected an agent error (no Ollama server), got reply %q", reply)
	}
	if strings.Contains(reply, "paused") {
		t.Errorf("owner reply = %q, must not be the paused reply", reply)
	}
	// /pause off resumes chat 77.
	reply, _ = turnAs(t, fn, 77, 77, "/pause off")
	if !strings.Contains(reply, "Resumed") {
		t.Errorf("/pause off reply = %q, want the resumed reply", reply)
	}
}

// --- Approval callback handler ------------------------------------------

// rc8CallbackFake is a fake Bot API for the callback-handler test:
// it records answerCallbackQuery and editMessageText calls.
type rc8CallbackFake struct {
	mu       sync.Mutex
	answered []string
	edits    []string
}

func (f *rc8CallbackFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
		var req struct {
			CallbackQueryID string `json:"callback_query_id"`
			Text            string `json:"text"`
		}
		_ = json.Unmarshal(body, &req)
		f.answered = append(f.answered, req.CallbackQueryID+":"+req.Text)
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		f.edits = append(f.edits, string(body))
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"description":"not found"}`)
	}
}

func TestApprovalCallbackRoundTrip(t *testing.T) {
	fake := &rc8CallbackFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL("")
	gw, err := messaging.NewTelegramGateway("123456:AAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	ownerID := int64(7429947930)
	// Start a Check that waits on the user's answer: the prompter
	// captures the request and returns at once (the "prompt" is
	// already out there, as far as the gate is concerned).
	reqCh := make(chan permissions.ApprovalRequest, 1)
	gate := permissions.NewGate(permissions.NewRuleStore(""), func(ctx context.Context, req permissions.ApprovalRequest) error {
		reqCh <- req
		return nil
	})
	resultCh := make(chan bool, 1)
	go func() {
		ctx := permissions.WithApprovalContext(context.Background(), 77, 77)
		ok, _ := gate.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var req permissions.ApprovalRequest
	select {
	case req = <-reqCh:
	case <-time.After(5 * time.Second):
		t.Fatal("gate never prompted")
	}

	// A stranger's tap is refused and the request stays pending.
	q := messaging.TGCallbackQuery{
		ID:      "cb-stranger",
		From:    &messaging.TGUser{ID: 5},
		Message: &messaging.TGMessage{MessageID: 9, Chat: messaging.TGChat{ID: 77}},
		Data:    "perm:approve:" + req.ID,
	}
	if err := handleApprovalCallback(context.Background(), gw, gate, ownerID, q, nil); err != nil {
		t.Fatalf("stranger tap: %v", err)
	}
	select {
	case <-resultCh:
		t.Fatal("stranger's tap resolved the approval")
	default:
	}
	// The requester approves: Check returns true.
	q.From = &messaging.TGUser{ID: 77}
	q.ID = "cb-owner"
	if err := handleApprovalCallback(context.Background(), gw, gate, ownerID, q, nil); err != nil {
		t.Fatalf("requester tap: %v", err)
	}
	select {
	case ok := <-resultCh:
		if !ok {
			t.Error("Check returned false after the requester approved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after approval")
	}
	// The query was answered and the prompt edited in place.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.answered) != 2 {
		t.Errorf("answered callbacks = %d, want 2 (stranger refusal + approval)", len(fake.answered))
	}
	if len(fake.edits) != 1 {
		t.Errorf("prompt edits = %d, want 1 (the approval outcome)", len(fake.edits))
	}
	// F-A5-P2-21: the resolved card must not keep live buttons — the
	// outcome edit carries an empty inline keyboard, exactly like
	// the expiry path, so a second tap cannot look live.
	if len(fake.edits) == 1 && !strings.Contains(fake.edits[0], `"inline_keyboard":[]`) {
		t.Errorf("outcome edit body = %s, want it to carry an empty inline_keyboard", fake.edits[0])
	}
}

func TestApprovalCallbackExpiredRequest(t *testing.T) {
	fake := &rc8CallbackFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL("")
	gw, err := messaging.NewTelegramGateway("123456:AAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	gate := permissions.NewGate(permissions.NewRuleStore(""), nil)
	q := messaging.TGCallbackQuery{
		ID:      "cb-1",
		From:    &messaging.TGUser{ID: 77},
		Message: &messaging.TGMessage{MessageID: 9, Chat: messaging.TGChat{ID: 77}},
		Data:    "perm:approve:0123456789abcdef",
	}
	if err := handleApprovalCallback(context.Background(), gw, gate, 7429947930, q, nil); err != nil {
		t.Fatalf("expired tap: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.answered) != 1 || !strings.Contains(fake.answered[0], "expired") {
		t.Errorf("answered = %v, want one 'expired' answer", fake.answered)
	}
}
