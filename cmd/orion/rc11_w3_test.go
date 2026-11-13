// W3 (rc11) regression tests for Audit 6 F-A6-3: `orion daemon`
// and `orion telegram poll` never called SetPermissionGate, so
// destructive/network tools ran without approval on those paths,
// and `telegram poll` additionally used the global a.Handle,
// bypassing per-chat isolation. The tests prove the wiring at
// three levels: wireGate installs a non-nil gate whose approval
// card round-trips through handleApprovalCallback; the shared
// daemonTurnHandler sets the approval context (so spawned
// subagents are attributed to the chat); and both commands, run
// end to end against a fake Bot API + fake provider, send an
// approval card for a shell_exec call instead of running it
// ungated.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/subagent"
	"github.com/irfan/orion/internal/tools"
)

// rc11FakeBot is a minimal Telegram Bot API fake: getUpdates serves
// a queued list of updates, sendMessage records the text and
// keyboard and returns a fresh message_id, editMessageText and
// answerCallbackQuery record their calls, getMe/sendChatAction/
// setMyCommands answer ok. Only the method's last path segment
// matters — the token in the path is never inspected.
type rc11FakeBot struct {
	mu       sync.Mutex
	queue    []messaging.Update
	sent     []rc11BotSent
	edits    []rc11BotEdit
	answered []rc11BotAnswer
	getMe    int
	nextID   int64
}

type rc11BotSent struct {
	ChatID   int64
	Text     string
	Keyboard string // raw reply_markup JSON, "" when no keyboard
	MsgID    int64
}

type rc11BotEdit struct {
	ChatID int64
	MsgID  int64
	Text   string
}

type rc11BotAnswer struct {
	CallbackID string
	Text       string
}

func (f *rc11FakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	method := parts[len(parts)-1]
	switch method {
	case "getUpdates":
		f.mu.Lock()
		updates := f.queue
		f.queue = nil
		f.mu.Unlock()
		if len(updates) == 0 {
			// Long-polling against a real server blocks; the
			// fake answers at once, so pace the empty loop
			// instead of letting PollStream spin hot.
			time.Sleep(10 * time.Millisecond)
		}
		f.write(w, updates)
	case "sendMessage":
		var body struct {
			ChatID      int64           `json:"chat_id"`
			Text        string          `json:"text"`
			ReplyMarkup json.RawMessage `json:"reply_markup"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		f.mu.Lock()
		f.nextID++
		msgID := f.nextID
		f.sent = append(f.sent, rc11BotSent{ChatID: body.ChatID, Text: body.Text, Keyboard: string(body.ReplyMarkup), MsgID: msgID})
		f.mu.Unlock()
		f.write(w, map[string]any{"message_id": msgID})
	case "editMessageText":
		var body struct {
			ChatID    int64  `json:"chat_id"`
			MessageID int64  `json:"message_id"`
			Text      string `json:"text"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		f.mu.Lock()
		f.edits = append(f.edits, rc11BotEdit{ChatID: body.ChatID, MsgID: body.MessageID, Text: body.Text})
		f.mu.Unlock()
		f.write(w, map[string]any{"message_id": body.MessageID})
	case "answerCallbackQuery":
		var body struct {
			CallbackID string `json:"callback_query_id"`
			Text       string `json:"text"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		f.mu.Lock()
		f.answered = append(f.answered, rc11BotAnswer{CallbackID: body.CallbackID, Text: body.Text})
		f.mu.Unlock()
		f.write(w, true)
	case "getMe":
		f.mu.Lock()
		f.getMe++
		f.mu.Unlock()
		f.write(w, map[string]any{"id": 123456, "username": "TestBot", "is_bot": true})
	case "sendChatAction", "setMyCommands":
		f.write(w, true)
	default:
		http.Error(w, "unknown method "+method, http.StatusNotFound)
	}
}

func (f *rc11FakeBot) write(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *rc11FakeBot) enqueue(u messaging.Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, u)
}

// approvalCard returns the request ID and card message ID of the
// first sent approval card for chatID, or ("", 0) when none has
// been sent yet. The keyboard's first button carries
// "perm:approve:<requestID>".
func (f *rc11FakeBot) approvalCard(chatID int64) (string, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sent {
		if s.ChatID != chatID || !strings.Contains(s.Keyboard, "perm:approve:") {
			continue
		}
		var kb messaging.InlineKeyboardMarkup
		if err := json.Unmarshal([]byte(s.Keyboard), &kb); err != nil || len(kb.InlineKeyboard) == 0 || len(kb.InlineKeyboard[0]) == 0 {
			continue
		}
		if _, id, ok := messaging.ParsePermissionCallback(kb.InlineKeyboard[0][0].CallbackData); ok {
			return id, s.MsgID
		}
	}
	return "", 0
}

func (f *rc11FakeBot) answeredText(callbackID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.answered {
		if a.CallbackID == callbackID {
			return a.Text, true
		}
	}
	return "", false
}

// rc11WaitFor polls cond until it is true or the deadline passes.
// It fails the test with desc when the deadline passes, so a
// missing card / missing answer is a test failure, not a hang.
func rc11WaitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// rc11FakeProvider is a fake OpenAI-compatible provider: the first
// call of a turn (no tool-result message yet) returns a shell_exec
// tool call, and any call after a tool result returns the final
// answer. Keying on the request shape — not a raw call counter —
// matters because recall/extraction side calls also hit this
// endpoint and must not consume the scripted turn.
type rc11FakeProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *rc11FakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/chat/completions" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	data, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(data, &req)
	hasToolResult := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			hasToolResult = true
		}
	}
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if hasToolResult {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		return
	}
	_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"echo proof-hi\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
}

func (p *rc11FakeProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// rc11SpawnScriptProvider answers the first Chat call with a
// subagent_spawn tool call and every later call with "ok", so the
// daemonTurnHandler approval-context test can prove the spawned
// task is attributed to the chat that asked for it.
type rc11SpawnScriptProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *rc11SpawnScriptProvider) Name() string { return "stub" }

func (p *rc11SpawnScriptProvider) Chat(_ context.Context, _ model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		return &model.ChatResponse{
			Message: model.Message{
				Role:      "assistant",
				ToolCalls: []model.ToolCall{{ID: "c1", Name: "subagent_spawn", Arguments: `{"brief":"count the words"}`}},
			},
			FinishReason: "tool_calls",
			Usage:        model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
		}, nil
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "ok"},
		FinishReason: "stop",
		Usage:        model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
	}, nil
}

func (p *rc11SpawnScriptProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := fn(resp.Message.Content); err != nil {
		return nil, err
	}
	return resp, nil
}

// TestW3WireGateApprovesRoundTrip proves the F-A6-3 wiring at the
// unit level: wireGate returns a non-nil gate that is installed on
// the app, a shell_exec Check sends an approval card through the
// gateway, a stranger's tap is refused without touching the
// pending request, and the requester's tap resolves the Check.
func TestW3WireGateApprovesRoundTrip(t *testing.T) {
	bot := &rc11FakeBot{}
	srv := httptest.NewServer(bot)
	defer srv.Close()
	oldBase := messaging.DefaultBaseURLValue()
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL(oldBase)

	a := daemonTestApp(t, &daemonScriptProvider{})
	defer a.Close()
	a.Budget = budget.New(100000)
	a.Config.TelegramToken = "123456:AAAAAAAAAAAAAAAAAAAA"
	a.Config.TelegramAllowedChats = []int64{111}
	gw, err := newTelegramGateway(a)
	if err != nil {
		t.Fatalf("newTelegramGateway: %v", err)
	}
	gate, cards, err := wireGate(a, gw)
	if err != nil {
		t.Fatalf("wireGate: %v", err)
	}
	if gate == nil {
		t.Fatal("wireGate returned a nil gate — a Telegram loop built from it would run ungated")
	}
	if a.PermissionGate() != gate {
		t.Fatal("wireGate did not install the gate on the app (PermissionGate() != returned gate)")
	}

	ctx := permissions.WithApprovalContext(context.Background(), 111, 111)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := gate.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var reqID string
	var cardMsgID int64
	rc11WaitFor(t, "approval card for chat 111", func() bool {
		reqID, cardMsgID = bot.approvalCard(111)
		return reqID != ""
	})
	if cardMsgID == 0 {
		t.Fatal("approval card arrived with no message id")
	}
	if gate.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1 while the card is unanswered", gate.PendingCount())
	}

	// A stranger (neither requester nor owner) may neither approve
	// nor deny: the tap is refused and the request stays pending.
	stranger := messaging.TGCallbackQuery{
		ID:   "cb-stranger",
		From: &messaging.TGUser{ID: 999},
		Message: &messaging.TGMessage{
			MessageID: cardMsgID,
			Chat:      messaging.TGChat{ID: 111},
		},
		Data: "perm:approve:" + reqID,
	}
	if err := handleApprovalCallback(context.Background(), gw, gate, 111, stranger, cards); err != nil {
		t.Fatalf("stranger tap: %v", err)
	}
	if text, ok := bot.answeredText("cb-stranger"); !ok || !strings.Contains(text, "Only the person who asked") {
		t.Errorf("stranger tap answer = %q (answered=%v), want the refusal toast", text, ok)
	}
	if gate.PendingCount() != 1 {
		t.Errorf("PendingCount after stranger tap = %d, want 1 (the request must stay pending)", gate.PendingCount())
	}

	// The requester's tap approves: Check returns true, the card
	// is edited in place with the outcome, and the query is
	// answered.
	approve := messaging.TGCallbackQuery{
		ID:   "cb-approve",
		From: &messaging.TGUser{ID: 111},
		Message: &messaging.TGMessage{
			MessageID: cardMsgID,
			Chat:      messaging.TGChat{ID: 111},
		},
		Data: "perm:approve:" + reqID,
	}
	if err := handleApprovalCallback(context.Background(), gw, gate, 111, approve, cards); err != nil {
		t.Fatalf("requester tap: %v", err)
	}
	select {
	case ok := <-resultCh:
		if !ok {
			t.Error("Check returned false after the requester's approve tap, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the approve tap")
	}
	if _, found := gate.Pending(reqID); found {
		t.Error("request still pending after the approve tap resolved it")
	}
	bot.mu.Lock()
	edited := false
	for _, e := range bot.edits {
		if e.MsgID == cardMsgID && strings.Contains(e.Text, "Approved") {
			edited = true
		}
	}
	bot.mu.Unlock()
	if !edited {
		t.Error("approval card was not edited in place with the Approved outcome")
	}
	if text, ok := bot.answeredText("cb-approve"); !ok || !strings.Contains(text, "Approved") {
		t.Errorf("approve tap answer = %q (answered=%v), want the Approved toast", text, ok)
	}
}

// TestW3DaemonTurnHandlerSetsApprovalContext proves the shared
// turn handler (used by both `orion daemon` and `orion telegram
// poll`) sets the approval context before the turn runs: a
// subagent spawned in the turn is attributed to the chat, which
// is what makes the gate address its card to the right chat and
// the sweep charge the right chat's budget.
func TestW3DaemonTurnHandlerSetsApprovalContext(t *testing.T) {
	prov := &rc11SpawnScriptProvider{}
	a := daemonTestApp(t, &daemonScriptProvider{})
	defer a.Close()
	a.Budget = budget.New(100000)
	// Per-chat agents are built lazily from a.Provider
	// (buildChatProviderLocked), so swapping it here points the
	// turn at the spawning script; the global agent keeps the
	// dummy provider, which the per-chat path never uses.
	a.Provider = prov
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return "child done", subagent.Usage{Tokens: 5}, nil
	})
	a.Subagents = mgr
	subTools, err := tools.NewSubagentTools(mgr, tools.UsageSinkFunc(func(int) {}))
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	for _, tool := range subTools {
		if err := a.Tools.Register(tool); err != nil {
			t.Fatalf("Register %s: %v", tool.Name(), err)
		}
	}

	handler := daemonTurnHandler(a)
	ctx := messaging.WithSenderID(context.Background(), 111)
	reply, err := handler(ctx, 111, "delegate this")
	if err != nil {
		t.Fatalf("daemonTurnHandler: %v", err)
	}
	if reply != "ok" {
		t.Errorf("daemon turn reply = %q, want %q", reply, "ok")
	}
	tasks := mgr.List()
	if len(tasks) != 1 {
		t.Fatalf("spawned tasks = %d, want 1", len(tasks))
	}
	if tasks[0].ChatID != 111 {
		t.Errorf("spawned task ChatID = %d, want 111 (the handler must set the approval context before the turn)", tasks[0].ChatID)
	}
}

// rc11WriteDaemonTestConfig writes a config file for the
// end-to-end gate tests: the fake provider as deepseek, the fake
// bot token, allowlist [111], sandbox off (no launcher needed).
func rc11WriteDaemonTestConfig(t *testing.T, home string, providerURL string) {
	t.Helper()
	body := `{
  "default_provider": "deepseek",
  "providers": {"deepseek": {"api_key": "fake-test-key", "base_url": "` + providerURL + `", "model": "deepseek-chat"}},
  "telegram_token": "123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
  "telegram_allowed_chats": [111],
  "data_dir": "` + home + `/.orion",
  "max_steps": 5,
  "timeout_seconds": 30,
  "sandbox": {"enabled": false}
}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestW3DaemonAndTelegramPollGatePrompts is the end-to-end F-A6-3
// proof for both commands that start a Telegram loop: with a
// message from an allowlisted chat asking for a shell_exec, the
// command must send an approval card — it must not run the tool
// ungated (the pre-rc11 behaviour on both paths). The turn is
// cancelled while it waits for the approval, so no approval tap
// is needed here: the card itself is the proof the gate is
// installed and prompting. (The tap → resolve path is covered by
// TestW3WireGateApprovesRoundTrip, which calls
// handleApprovalCallback directly and does not depend on
// PollStream's callback dispatch.)
func TestW3DaemonAndTelegramPollGatePrompts(t *testing.T) {
	for _, cmd := range []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"daemon", func(ctx context.Context) error { return cmdDaemon(ctx, nil) }},
		{"telegram poll", func(ctx context.Context) error { return cmdTelegram(ctx, []string{"poll"}) }},
	} {
		t.Run(cmd.name, func(t *testing.T) {
			home := scratchHome(t)
			provider := &rc11FakeProvider{}
			provSrv := httptest.NewServer(provider)
			defer provSrv.Close()
			bot := &rc11FakeBot{}
			botSrv := httptest.NewServer(bot)
			defer botSrv.Close()
			rc11WriteDaemonTestConfig(t, home, provSrv.URL)
			t.Setenv("ORION_TELEGRAM_BASE_URL", botSrv.URL)

			// The message is queued before polling starts, so
			// the first getUpdates delivers it.
			bot.enqueue(messaging.Update{
				UpdateID: 1,
				Message: &messaging.TGMessage{
					MessageID: 1,
					Chat:      messaging.TGChat{ID: 111},
					Text:      "please run echo proof-hi",
					From:      &messaging.TGUser{ID: 111},
				},
			})
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() {
				_, err := capture(func() error { return cmd.run(ctx) })
				errCh <- err
			}()
			// The gate must prompt: an approval card for chat
			// 111, naming the shell_exec call. Without wireGate
			// there is no card at all — the tool would run
			// ungated instead — so this wait is the proof.
			var reqID string
			rc11WaitFor(t, "approval card for chat 111", func() bool {
				reqID, _ = bot.approvalCard(111)
				return reqID != ""
			})
			if provider.Calls() == 0 {
				t.Error("provider was never called — the turn never started")
			}
			// Cancel while the turn waits for the approval:
			// the gate denies on cancellation, the turn ends,
			// and Poll returns nil cleanly.
			cancel()
			select {
			case err := <-errCh:
				if err != nil {
					t.Errorf("command after cancel: err = %v, want nil (Poll returns nil on ctx cancellation)", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("command did not return after cancelling its context")
			}
			// The card must name the tool call it is gating.
			bot.mu.Lock()
			var cardText string
			for _, s := range bot.sent {
				if strings.Contains(s.Keyboard, "perm:approve:"+reqID) {
					cardText = s.Text
				}
			}
			getMeCalls := bot.getMe
			bot.mu.Unlock()
			if !strings.Contains(cardText, "shell_exec") {
				t.Errorf("approval card text = %q, want it to name shell_exec", cardText)
			}
			if cmd.name == "daemon" && getMeCalls == 0 {
				t.Error("daemon never called getMe (it must verify the token before polling)")
			}
		})
	}
}
