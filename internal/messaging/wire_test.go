// WireTelegram discovery tests (internal/messaging): a tool that
// implements tools.CallbackHandler gets its declared prefix
// registered on the router, and a tool that implements
// tools.ReplyInterceptor is added to the reply chain — with no
// per-tool call site. A bad declaration (duplicate prefix,
// 16-byte cap, "perm:" reservation) fails the whole wiring
// loudly at startup. The question transport stays
// messaging-owned (its pending-question state lives here), so
// these tests use fake tools whose handler/interceptor state
// lives in the tool itself — the generic path the next
// interactive tool will use.
package messaging

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// wireCallbackTool is a minimal tool implementing
// tools.CallbackHandler: it records the callback data it
// receives. Its prefix is configurable, so the tests can pin the
// duplicate/cap/reservation rules. It answers nothing itself —
// dispatch (the tap reaching the tool) is what is under test;
// the answer path is covered by the question/approval tests.
type wireCallbackTool struct {
	name   string
	prefix string
	mu     sync.Mutex
	taps   []string
}

func (t *wireCallbackTool) Name() string { return t.name }
func (t *wireCallbackTool) Description() string {
	return "test callback tool"
}
func (t *wireCallbackTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: t.name, Description: t.Description(), Parameters: map[string]any{"type": "object"}}
}
func (t *wireCallbackTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}
func (t *wireCallbackTool) RiskClass() permissions.Class { return permissions.ClassSafe }
func (t *wireCallbackTool) CallbackPrefix() string       { return t.prefix }
func (t *wireCallbackTool) HandleCallback(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.taps = append(t.taps, q.CallbackData())
	return nil
}
func (t *wireCallbackTool) tapData() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.taps...)
}

// wireInterceptorTool is a minimal tool implementing
// tools.ReplyInterceptor: it records the updates it sees and
// returns its configured answer.
type wireInterceptorTool struct {
	name   string
	answer bool
	mu     sync.Mutex
	seen   []string
}

func (t *wireInterceptorTool) Name() string { return t.name }
func (t *wireInterceptorTool) Description() string {
	return "test reply interceptor tool"
}
func (t *wireInterceptorTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: t.name, Description: t.Description(), Parameters: map[string]any{"type": "object"}}
}
func (t *wireInterceptorTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}
func (t *wireInterceptorTool) RiskClass() permissions.Class { return permissions.ClassSafe }
func (t *wireInterceptorTool) TryAnswer(upd tools.ReplyUpdate) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen = append(t.seen, upd.Text())
	return t.answer
}
func (t *wireInterceptorTool) seenTexts() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.seen...)
}

// wireTestGateway builds a gateway + gate + cards for a
// WireTelegram test, on a fake Telegram server. The wiring
// itself makes no network calls, but the gateway needs a base
// URL — and the question handler's answer path (for ask: taps)
// goes through the fake server, which records answers.
func wireTestGateway(t *testing.T) (*TelegramGateway, *permissions.Gate, *ApprovalCards) {
	t.Helper()
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	gate := permissions.NewGate(permissions.NewRuleStore(""), nil)
	return gw, gate, NewApprovalCards()
}

// wireTap invokes the gateway's installed callback handler with
// one tap (exactly as PollStream would deliver it).
func wireTap(t *testing.T, gw *TelegramGateway, data string) error {
	t.Helper()
	h := gw.callbackHandlerFor()
	if h == nil {
		t.Fatal("WireTelegram did not install a callback handler on the gateway")
	}
	return h(context.Background(), TGCallbackQuery{
		ID:      "q1",
		Data:    data,
		From:    &TGUser{ID: 77},
		Message: &TGMessage{MessageID: 5, Chat: TGChat{ID: 77}},
	})
}

// TestWireTelegramDiscoversCallbackHandler pins the F-A20-1
// contract: a tool implementing tools.CallbackHandler gets its
// declared prefix registered, and a tap with that prefix reaches
// the tool through the installed handler. An "ask:" tap must
// still go to the question handler, never to the tool.
func TestWireTelegramDiscoversCallbackHandler(t *testing.T) {
	gw, gate, cards := wireTestGateway(t)
	reg := tools.NewRegistry()
	tool := &wireCallbackTool{name: "fake_tool", prefix: "fake:"}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("Register fake_tool: %v", err)
	}
	if err := WireTelegram(reg, gw, gate, cards, 77, "test"); err != nil {
		t.Fatalf("WireTelegram: %v", err)
	}
	if err := wireTap(t, gw, "fake:payload"); err != nil {
		t.Fatalf("tap fake:payload: %v", err)
	}
	if got := tool.tapData(); len(got) != 1 || got[0] != "fake:payload" {
		t.Fatalf("tool taps = %v, want [fake:payload]", got)
	}
	// An "ask:" tap goes to the question handler, not the tool:
	// the question is unknown, so it is answered as expired
	// through the fake server, and the tool sees nothing new.
	if err := wireTap(t, gw, "ask:deadbeef:0"); err != nil {
		t.Fatalf("tap ask:deadbeef:0: %v", err)
	}
	if got := tool.tapData(); len(got) != 1 {
		t.Fatalf("tool taps after ask: tap = %v, want still [fake:payload]", got)
	}
	// A "perm:" tap goes to the approval fallback, never to the
	// tool: the request is unknown, so it is answered as "no
	// such approval" through the fake server.
	if err := wireTap(t, gw, "perm:approve:deadbeef"); err != nil {
		t.Fatalf("tap perm:approve:deadbeef: %v", err)
	}
	if got := tool.tapData(); len(got) != 1 {
		t.Fatalf("tool taps after perm: tap = %v, want still [fake:payload]", got)
	}
}

// TestWireTelegramDiscoversReplyInterceptor pins the other half
// of the F-A20-1 contract: a tool implementing
// tools.ReplyInterceptor is added to the reply chain, and a
// typed reply it claims is consumed (no turn starts). A tool
// that declines leaves the update unconsumed.
func TestWireTelegramDiscoversReplyInterceptor(t *testing.T) {
	gw, gate, cards := wireTestGateway(t)
	reg := tools.NewRegistry()
	tool := &wireInterceptorTool{name: "fake_tool", answer: true}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("Register fake_tool: %v", err)
	}
	if err := WireTelegram(reg, gw, gate, cards, 77, "test"); err != nil {
		t.Fatalf("WireTelegram: %v", err)
	}
	upd := Update{Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hello", From: &TGUser{ID: 77}}}
	if !gw.replyChainTryAnswer(upd) {
		t.Fatal("replyChainTryAnswer did not consume the reply the tool claimed")
	}
	if got := tool.seenTexts(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("tool saw %v, want [hello]", got)
	}
	// A declining interceptor leaves the update unconsumed.
	gw2, gate2, cards2 := wireTestGateway(t)
	reg2 := tools.NewRegistry()
	decliner := &wireInterceptorTool{name: "fake_tool", answer: false}
	if err := reg2.Register(decliner); err != nil {
		t.Fatalf("Register decliner: %v", err)
	}
	if err := WireTelegram(reg2, gw2, gate2, cards2, 77, "test"); err != nil {
		t.Fatalf("WireTelegram: %v", err)
	}
	if gw2.replyChainTryAnswer(upd) {
		t.Fatal("replyChainTryAnswer consumed a reply the tool declined")
	}
	if got := decliner.seenTexts(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("decliner saw %v, want [hello]", got)
	}
}

// TestWireTelegramRejectsDuplicateCallbackPrefix pins that two
// tools declaring the same prefix fail the wiring loudly —
// never silently overwriting each other — and that a tool may
// not claim "ask:" (the question transport owns it).
func TestWireTelegramRejectsDuplicateCallbackPrefix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prefixes []string
	}{
		{"two tools, one prefix", []string{"dup:", "dup:"}},
		{"tool claims ask:", []string{"ask:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, gate, cards := wireTestGateway(t)
			reg := tools.NewRegistry()
			for i, p := range tc.prefixes {
				tool := &wireCallbackTool{name: "fake_tool_" + string(rune('a'+i)), prefix: p}
				if err := reg.Register(tool); err != nil {
					t.Fatalf("Register %s: %v", tool.name, err)
				}
			}
			if err := WireTelegram(reg, gw, gate, cards, 77, "test"); err == nil {
				t.Fatalf("WireTelegram with prefixes %v: accepted, want a loud error", tc.prefixes)
			}
		})
	}
}

// TestWireTelegramRejectsLongCallbackPrefix pins the 16-byte cap:
// a 17-byte prefix fails loudly, and a 16-byte prefix (the
// boundary) is accepted.
func TestWireTelegramRejectsLongCallbackPrefix(t *testing.T) {
	// 17 bytes: 16 digits + ":".
	tooLong := &wireCallbackTool{name: "fake_tool", prefix: "1234567890123456:"}
	gw, gate, cards := wireTestGateway(t)
	reg := tools.NewRegistry()
	if err := reg.Register(tooLong); err != nil {
		t.Fatalf("Register tooLong: %v", err)
	}
	if err := WireTelegram(reg, gw, gate, cards, 77, "test"); err == nil {
		t.Fatal("WireTelegram with a 17-byte prefix: accepted, want a loud error")
	}
	// 16 bytes: 15 digits + ":" — the boundary, accepted.
	atCap := &wireCallbackTool{name: "fake_tool", prefix: "123456789012345:"}
	gw2, gate2, cards2 := wireTestGateway(t)
	reg2 := tools.NewRegistry()
	if err := reg2.Register(atCap); err != nil {
		t.Fatalf("Register atCap: %v", err)
	}
	if err := WireTelegram(reg2, gw2, gate2, cards2, 77, "test"); err != nil {
		t.Fatalf("WireTelegram with a 16-byte prefix: %v, want accepted", err)
	}
	if err := wireTap(t, gw2, "123456789012345:payload"); err != nil {
		t.Fatalf("tap at-cap prefix: %v", err)
	}
	if got := atCap.tapData(); len(got) != 1 || got[0] != "123456789012345:payload" {
		t.Fatalf("at-cap tool taps = %v, want [123456789012345:payload]", got)
	}
}

// TestWireTelegramDirect pins the F-A20-7 cases: nil args, and
// a second call on the same gateway — all loud fail. The first
// wiring still works after the failed second call (the duplicate
// "ask:" fails before any state changes).
func TestWireTelegramDirect(t *testing.T) {
	gw, gate, cards := wireTestGateway(t)
	reg := tools.NewRegistry()
	if err := WireTelegram(nil, gw, gate, cards, 77, "test"); err == nil {
		t.Error("WireTelegram(nil registry): accepted, want a loud error")
	}
	if err := WireTelegram(reg, nil, gate, cards, 77, "test"); err == nil {
		t.Error("WireTelegram(nil gateway): accepted, want a loud error")
	}
	if err := WireTelegram(reg, gw, nil, cards, 77, "test"); err == nil {
		t.Error("WireTelegram(nil gate): accepted, want a loud error")
	}
	if err := WireTelegram(reg, gw, gate, nil, 77, "test"); err == nil {
		t.Error("WireTelegram(nil cards): accepted, want a loud error")
	}
	// A nil registry must fail before touching the gateway: no
	// handler is installed by the failed calls above.
	if h := gw.callbackHandlerFor(); h != nil {
		t.Fatal("gateway has an installed callback handler after failed nil-arg calls, want none")
	}
	if err := WireTelegram(reg, gw, gate, cards, 77, "test"); err != nil {
		t.Fatalf("first WireTelegram: %v", err)
	}
	err := WireTelegram(reg, gw, gate, cards, 77, "test")
	if err == nil {
		t.Fatal("second WireTelegram on one gateway: accepted, want a loud error (duplicate ask:)")
	}
	if !strings.Contains(err.Error(), "ask:") {
		t.Fatalf("second WireTelegram error %v, want it to name the duplicate \"ask:\" prefix", err)
	}
	// The first wiring still works after the failed second call:
	// an ask: tap is still answered (unknown question → expired).
	if err := wireTap(t, gw, "ask:deadbeef:0"); err != nil {
		t.Fatalf("tap after failed second WireTelegram: %v", err)
	}
}

// TestWireTelegramPartialFailure pins the partial-failure
// window: a failure in the tool callback discovery leaves the
// gateway partially wired (the "ask:" handler and any tool
// handlers registered before the failure stay registered) — and
// the failure is loud, so no binary ever serves traffic on it:
// the router was never installed as the gateway's handler.
func TestWireTelegramPartialFailure(t *testing.T) {
	gw, gate, cards := wireTestGateway(t)
	reg := tools.NewRegistry()
	// The first tool registers fine; the second duplicates its
	// prefix, so the discovery loop fails on it.
	good := &wireCallbackTool{name: "good_tool", prefix: "good:"}
	bad := &wireCallbackTool{name: "bad_tool", prefix: "good:"}
	if err := reg.Register(good); err != nil {
		t.Fatalf("Register good_tool: %v", err)
	}
	if err := reg.Register(bad); err != nil {
		t.Fatalf("Register bad_tool: %v", err)
	}
	err := WireTelegram(reg, gw, gate, cards, 77, "test")
	if err == nil {
		t.Fatal("WireTelegram with a duplicate tool prefix: accepted, want a loud error")
	}
	if !strings.Contains(err.Error(), "good:") {
		t.Fatalf("WireTelegram error %v, want it to name the duplicate prefix", err)
	}
	// Partial state: "ask:" and "good:" are registered on the
	// router, but the router was never installed as the
	// gateway's handler (the failure came before
	// InstallCallbackRouter), so no tap can be dispatched.
	// Both binaries kill the process on a WireTelegram error
	// instead of serving traffic on a partially wired gateway.
	if h := gw.callbackHandlerFor(); h != nil {
		t.Fatal("gateway has an installed callback handler after a failed WireTelegram, want none (partial wiring must not serve traffic)")
	}
}

// TestWireTelegramRejectsPermPrefix pins that "perm:" is reserved
// for the approval fallback and can never be registered as a
// tool prefix.
func TestWireTelegramRejectsPermPrefix(t *testing.T) {
	gw, gate, cards := wireTestGateway(t)
	reg := tools.NewRegistry()
	tool := &wireCallbackTool{name: "fake_tool", prefix: "perm:"}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("Register fake_tool: %v", err)
	}
	if err := WireTelegram(reg, gw, gate, cards, 77, "test"); err == nil {
		t.Fatal("WireTelegram with prefix \"perm:\": accepted, want a loud error (reserved for approvals)")
	}
	// The error message must name the reservation, so a
	// developer reading the startup failure knows why.
	gw2, gate2, cards2 := wireTestGateway(t)
	reg2 := tools.NewRegistry()
	tool2 := &wireCallbackTool{name: "fake_tool", prefix: "perm:"}
	if err := reg2.Register(tool2); err != nil {
		t.Fatalf("Register fake_tool: %v", err)
	}
	err := WireTelegram(reg2, gw2, gate2, cards2, 77, "test")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("WireTelegram with prefix \"perm:\": error %v, want it to name the reservation", err)
	}
}
