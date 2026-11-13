package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// --- Scratch live proof (rc8): real daemon wiring, fake Bot API -------
//
// This test is the scratch proof the coordinator asked for: it runs
// the REAL PollStream + turn handler + permission gate + callback
// handler against a fake Bot API server (no live bot, no live
// config, no network beyond loopback). The only fakes are the Bot
// API itself and the model (a scripted provider that asks for one
// harmless shell_exec per turn). It proves:
//
//   - the gate's approve round-trip: chat 111's shell_exec is
//     prompted, approved by tapping ✅, and really executes;
//   - the gate's deny round-trip: chat 222's identical call is
//     prompted, denied by tapping ❌, and never executes — the
//     denial reaches the model as a tool result;
//   - two-chat isolation: neither chat's history contains the
//     other's turn.
//
// The approval prompt is answered by tapping the button Telegram
// would deliver: the test reads the request ID out of the keyboard
// the fake server received and enqueues a callback-query update
// for it, exactly as the Bot API would.

// rc8ProofFake is a fake Bot API server for the scratch proof. It
// serves scripted getUpdates from a queue the test enqueues into,
// and records every sendMessage (with keyboards), editMessageText,
// sendChatAction and answerCallbackQuery call.
type rc8ProofFake struct {
	mu       sync.Mutex
	queue    []messaging.Update
	sent     []rc8Sent
	edits    []string
	answered []string
	nextID   int64
}

type rc8Sent struct {
	ChatID   int64
	Text     string
	Keyboard string // raw reply_markup JSON, "" when none
}

func (f *rc8ProofFake) enqueue(u messaging.Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, u)
}

func (f *rc8ProofFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		out := f.queue
		f.queue = nil
		data, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		w.Write(data)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		var req struct {
			ChatID      int64           `json:"chat_id"`
			Text        string          `json:"text"`
			ReplyMarkup json.RawMessage `json:"reply_markup"`
		}
		_ = json.Unmarshal(body, &req)
		f.nextID++
		f.sent = append(f.sent, rc8Sent{ChatID: req.ChatID, Text: req.Text, Keyboard: string(req.ReplyMarkup)})
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, f.nextID)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		f.edits = append(f.edits, string(body))
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
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"orion_proof_bot","first_name":"Orion"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"description":"not found"}`)
	}
}

// rc8ScriptedProvider is a scripted model for the proof: every odd
// Chat call asks for one shell_exec ("echo proof-hi"), every even
// call answers "done". Turns run sequentially in the proof, so a
// single counter is enough.
type rc8ScriptedProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *rc8ScriptedProvider) Name() string { return "ollama" }
func (p *rc8ScriptedProvider) Chat(_ context.Context, _ model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls%2 == 1 {
		return &model.ChatResponse{
			Message: model.Message{
				Role:      "assistant",
				ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("call-%d", p.calls), Name: "shell_exec", Arguments: `{"command":"echo proof-hi"}`}},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "done"},
		FinishReason: "stop",
	}, nil
}
func (p *rc8ScriptedProvider) Stream(_ context.Context, _ model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	if err := fn("done"); err != nil {
		return nil, err
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "done"},
		FinishReason: "stop",
	}, nil
}

func TestScratchProofGateAndIsolation(t *testing.T) {
	dir := t.TempDir()
	fake := &rc8ProofFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	// The app: real wiring, scripted model, real shell tool (the
	// proof command is a harmless echo).
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

	// The gateway + gate + callback handler, exactly as run()
	// wires them (owner = first allowlisted chat).
	ownerID := int64(7429947930)
	gw, err := messaging.NewTelegramGateway("123456:AAAAAAAAAAAAAAAAAAAA",
		messaging.WithBaseURL(srv.URL),
		messaging.WithAllowedChats(ownerID, 111, 222))
	if err != nil {
		t.Fatal(err)
	}
	ruleStore := permissions.NewRuleStore(filepath.Join(dir, "permissions.json"))
	gate := permissions.NewGate(ruleStore, func(ctx context.Context, req permissions.ApprovalRequest) error {
		text := fmt.Sprintf("⚠️ Approval needed\n\n%s", req.Summary())
		_, err := gw.SendMessageWithKeyboard(ctx, req.ChatID, text, messaging.ApprovalKeyboard(req.ID))
		return err
	})
	a.SetPermissionGate(gate)
	gw.SetCallbackHandler(func(ctx context.Context, q messaging.TGCallbackQuery) error {
		return handleApprovalCallback(ctx, gw, gate, ownerID, q)
	})

	allowed := map[int64]bool{ownerID: true, 111: true, 222: true}
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
		deadline := time.Now().Add(15 * time.Second)
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
	// approveNext waits for the approval prompt for chatID, then
	// taps the given decision for it (reading the request ID out
	// of the keyboard, as the Bot API would deliver it).
	approveNext := func(chatID int64, decision string) {
		t.Helper()
		var requestID string
		waitFor(fmt.Sprintf("approval prompt for chat %d", chatID), func() bool {
			for _, s := range fake.sent {
				if s.ChatID != chatID || s.Keyboard == "" {
					continue
				}
				var kb messaging.InlineKeyboardMarkup
				if err := json.Unmarshal([]byte(s.Keyboard), &kb); err != nil {
					continue
				}
				for _, row := range kb.InlineKeyboard {
					for _, b := range row {
						if dec, id, ok := messaging.ParsePermissionCallback(b.CallbackData); ok && dec == decision {
							requestID = id
							return true
						}
					}
				}
			}
			return false
		})
		fake.enqueue(messaging.Update{UpdateID: time.Now().UnixNano(), CallbackQuery: &messaging.TGCallbackQuery{
			ID:      "cb-" + decision + "-" + requestID,
			From:    &messaging.TGUser{ID: chatID},
			Message: &messaging.TGMessage{MessageID: 1, Chat: messaging.TGChat{ID: chatID}},
			Data:    "perm:" + decision + ":" + requestID,
		}})
	}
	replyTo := func(chatID int64, prefix string) {
		t.Helper()
		waitFor(fmt.Sprintf("reply to chat %d", chatID), func() bool {
			for _, s := range fake.sent {
				if s.ChatID == chatID && s.Keyboard == "" && strings.HasPrefix(s.Text, prefix) {
					return true
				}
			}
			return false
		})
	}

	// --- Chat 111: approve. ---
	fake.enqueue(messaging.Update{UpdateID: 1, Message: &messaging.TGMessage{
		MessageID: 1, Chat: messaging.TGChat{ID: 111}, Text: "run the shell",
		From: &messaging.TGUser{ID: 111},
	}})
	approveNext(111, "approve")
	replyTo(111, "done")

	// --- Chat 222: deny. ---
	fake.enqueue(messaging.Update{UpdateID: 2, Message: &messaging.TGMessage{
		MessageID: 2, Chat: messaging.TGChat{ID: 222}, Text: "run the shell",
		From: &messaging.TGUser{ID: 222},
	}})
	approveNext(222, "deny")
	replyTo(222, "done")

	cancel()
	select {
	case err := <-pollErr:
		if err != nil {
			t.Fatalf("PollStream after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PollStream did not return after cancel")
	}

	// --- Assertions on the wire. ---
	fake.mu.Lock()
	sent := append([]rc8Sent(nil), fake.sent...)
	answered := append([]string(nil), fake.answered...)
	edits := append([]string(nil), fake.edits...)
	fake.mu.Unlock()
	prompts := 0
	for _, s := range sent {
		if s.Keyboard != "" {
			prompts++
		}
	}
	if prompts != 2 {
		t.Errorf("approval prompts sent = %d, want 2 (one per chat)", prompts)
	}
	if len(answered) != 2 {
		t.Errorf("callback queries answered = %d, want 2", len(answered))
	}
	// The approval handler edits the prompt in place with the
	// outcome ("Approval request: ..."). The terminal card also
	// edits messages, so count only the approval outcomes.
	outcomeEdits := 0
	for _, e := range edits {
		if strings.Contains(e, "Approval request:") {
			outcomeEdits++
		}
	}
	if outcomeEdits != 2 {
		t.Errorf("approval outcome edits = %d, want 2 (one per tap); all edits: %v", outcomeEdits, edits)
	}

	// --- Assertions on the histories. ---
	readChat := func(chat int64) []model.Message {
		t.Helper()
		fs, err := memory.NewFileStore(filepath.Join(dir, "chats", fmt.Sprint(chat)))
		if err != nil {
			t.Fatalf("read chat %d store: %v", chat, err)
		}
		defer fs.Close()
		msgs, err := fs.RecentMessages(0)
		if err != nil {
			t.Fatalf("read chat %d history: %v", chat, err)
		}
		return msgs
	}
	msgs111 := readChat(111)
	joined111 := ""
	for _, m := range msgs111 {
		joined111 += m.Role + ":" + m.Content + "\n"
	}
	if !strings.Contains(joined111, "proof-hi") {
		t.Errorf("chat 111 history missing the approved shell result: %q", joined111)
	}
	if strings.Contains(joined111, "permission denied") {
		t.Errorf("chat 111 history contains a denial (it approved): %q", joined111)
	}
	msgs222 := readChat(222)
	joined222 := ""
	for _, m := range msgs222 {
		joined222 += m.Role + ":" + m.Content + "\n"
	}
	if !strings.Contains(joined222, "permission denied") {
		t.Errorf("chat 222 history missing the denial tool result: %q", joined222)
	}
	if strings.Contains(joined222, "proof-hi") {
		t.Errorf("chat 222 history contains the shell result (it denied): %q", joined222)
	}
	// No cross-chat leakage either way.
	if strings.Contains(joined111, "run the shell") && strings.Count(joined111, "run the shell") != 1 {
		t.Errorf("chat 111 history has unexpected turns: %q", joined111)
	}
	if strings.Count(joined222, "run the shell") != 1 {
		t.Errorf("chat 222 history has unexpected turns: %q", joined222)
	}
	if len(msgs111) == 0 || len(msgs222) == 0 {
		t.Errorf("empty histories: chat111=%d msgs, chat222=%d msgs", len(msgs111), len(msgs222))
	}
}
