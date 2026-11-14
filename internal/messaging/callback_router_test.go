// Router security-rule tests (F-A20-8): the callback router's
// prefix rules, the perm: reservation, and the loud-fail
// replacement rules for the fallback and the installed handler
// had NO unit tests — Audit 20 proved it by subset bypass. These
// tests pin every rule at unit level, so a future change that
// silently re-opens a dispatch hole fails here first.
package messaging

import (
	"context"
	"testing"

	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// recordingCallbackHandler is a tools.CallbackHandler that records
// the callback data it was dispatched.
type recordingCallbackHandler struct {
	prefix string
	calls  []string
}

func (h *recordingCallbackHandler) CallbackPrefix() string { return h.prefix }

func (h *recordingCallbackHandler) HandleCallback(ctx context.Context, gw tools.CallbackGateway, q tools.CallbackQuery) error {
	h.calls = append(h.calls, q.CallbackData())
	return nil
}

func TestCallbackPrefixExtraction(t *testing.T) {
	cases := []struct {
		data string
		want string
	}{
		{"ask:id:0", "ask:"},
		{"perm:approve:abc", "perm:"},
		{"plan:approve:abc123", "plan:"},
		{"nodata", ""},
		{"", ""},
		{":x", ":"},
		{"a:b:c", "a:"},
	}
	for _, tc := range cases {
		if got := callbackPrefix(tc.data); got != tc.want {
			t.Errorf("callbackPrefix(%q) = %q, want %q", tc.data, got, tc.want)
		}
	}
}

func TestCallbackRouterRegisterRules(t *testing.T) {
	r := newCallbackRouter(nil)
	h := &recordingCallbackHandler{prefix: "foo:"}
	if err := r.register("foo:", h); err != nil {
		t.Fatalf("register valid prefix: %v", err)
	}
	// A duplicate prefix is a loud error, never a silent overwrite.
	if err := r.register("foo:", h); err == nil {
		t.Error("duplicate callback prefix accepted, want a loud error")
	}
	// The prefix must end with ":".
	if err := r.register("bar", h); err == nil {
		t.Error("prefix without trailing colon accepted, want a loud error")
	}
	if err := r.register("", h); err == nil {
		t.Error("empty prefix accepted, want a loud error")
	}
	// The prefix is at most 16 bytes (Telegram caps callback_data
	// at 64 bytes and the tool's own payload needs the rest).
	if err := r.register("12345678901234567:", h); err == nil {
		t.Error("18-byte prefix accepted, want a loud error")
	}
	if err := r.register("123456789012345:", h); err != nil {
		t.Errorf("16-byte prefix rejected: %v", err)
	}
	// "perm:" is reserved for the approval fallback: a tool must
	// never claim it.
	if err := r.register(permissionCallbackPrefix, h); err == nil {
		t.Error("reserved prefix \"perm:\" accepted, want a loud error")
	}
	// A nil handler is a loud error, never a silent hole.
	if err := r.register("bar:", nil); err == nil {
		t.Error("nil handler accepted, want a loud error")
	}
	var nilRouter *callbackRouter
	if err := nilRouter.register("bar:", h); err == nil {
		t.Error("nil router register accepted, want a loud error")
	}
}

func TestCallbackRouterFallbackLoudOnReplace(t *testing.T) {
	r := newCallbackRouter(nil)
	fb := func(ctx context.Context, q TGCallbackQuery) error { return nil }
	if err := r.setFallback(fb); err != nil {
		t.Fatalf("first setFallback: %v", err)
	}
	// Replacing the fallback is a loud error (F-A20-6): the
	// fallback IS the approval handler, so a second install
	// would silently re-route every unclaimed tap — including
	// perm: approvals — to a different handler.
	if err := r.setFallback(fb); err == nil {
		t.Error("second fallback install accepted, want a loud error")
	}
	if err := r.setFallback(nil); err == nil {
		t.Error("nil fallback accepted, want a loud error")
	}
	var nilRouter *callbackRouter
	if err := nilRouter.setFallback(fb); err == nil {
		t.Error("nil router setFallback accepted, want a loud error")
	}
}

func TestCallbackRouterDispatch(t *testing.T) {
	gw, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	ctx := context.Background()
	tool := &recordingCallbackHandler{prefix: "plan:"}
	var fallbackCalls []string
	fb := func(ctx context.Context, q TGCallbackQuery) error {
		fallbackCalls = append(fallbackCalls, q.Data)
		return nil
	}
	r := newCallbackRouter(fb)
	if err := r.register("plan:", tool); err != nil {
		t.Fatalf("register plan: %v", err)
	}
	// A registered tool prefix goes to that tool's handler.
	if err := r.dispatch(ctx, gw, TGCallbackQuery{ID: "q1", Data: "plan:approve:abc123"}); err != nil {
		t.Fatalf("dispatch plan: tap: %v", err)
	}
	if len(tool.calls) != 1 || tool.calls[0] != "plan:approve:abc123" {
		t.Errorf("tool calls = %v, want [plan:approve:abc123]", tool.calls)
	}
	if len(fallbackCalls) != 0 {
		t.Errorf("fallback called for a tool prefix: %v", fallbackCalls)
	}
	// Anything else — including perm: taps and unparseable data —
	// goes to the approval fallback.
	if err := r.dispatch(ctx, gw, TGCallbackQuery{ID: "q2", Data: "perm:approve:xyz"}); err != nil {
		t.Fatalf("dispatch perm: tap: %v", err)
	}
	if err := r.dispatch(ctx, gw, TGCallbackQuery{ID: "q3", Data: "nonsense"}); err != nil {
		t.Fatalf("dispatch unparseable tap: %v", err)
	}
	if len(fallbackCalls) != 2 || fallbackCalls[0] != "perm:approve:xyz" || fallbackCalls[1] != "nonsense" {
		t.Errorf("fallback calls = %v, want [perm:approve:xyz nonsense]", fallbackCalls)
	}
	if len(tool.calls) != 1 {
		t.Errorf("tool calls = %v after fallback taps, want exactly 1", tool.calls)
	}
	// A nil fallback with an unknown prefix is an error, never a
	// silently dropped tap.
	r2 := newCallbackRouter(nil)
	if err := r2.register("plan:", tool); err != nil {
		t.Fatalf("register plan: on nil-fallback router: %v", err)
	}
	if err := r2.dispatch(ctx, gw, TGCallbackQuery{ID: "q4", Data: "perm:approve:xyz"}); err == nil {
		t.Error("unknown prefix with nil fallback accepted, want an error")
	}
	var nilRouter *callbackRouter
	if err := nilRouter.dispatch(ctx, gw, TGCallbackQuery{ID: "q5", Data: "plan:approve:abc123"}); err == nil {
		t.Error("nil router dispatch accepted, want an error")
	}
}

func TestSetCallbackHandlerLoudOnReplace(t *testing.T) {
	gw, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	h1 := func(ctx context.Context, q TGCallbackQuery) error { return nil }
	h2 := func(ctx context.Context, q TGCallbackQuery) error { return nil }
	if err := gw.SetCallbackHandler(h1); err != nil {
		t.Fatalf("first SetCallbackHandler: %v", err)
	}
	// Replacing an installed handler is a loud error (F-A20-6):
	// a second installer would silently re-route every tap.
	if err := gw.SetCallbackHandler(h2); err == nil {
		t.Error("second callback handler install accepted, want a loud error")
	}
	if gw.callbackHandlerFor() == nil {
		t.Error("replacement attempt cleared the installed handler")
	}
	// A nil handler still removes it, and a fresh install after
	// removal works.
	if err := gw.SetCallbackHandler(nil); err != nil {
		t.Fatalf("SetCallbackHandler(nil): %v", err)
	}
	if gw.callbackHandlerFor() != nil {
		t.Error("SetCallbackHandler(nil) did not remove the handler")
	}
	if err := gw.SetCallbackHandler(h2); err != nil {
		t.Errorf("install after removal failed: %v", err)
	}
}

func TestSetApprovalFallbackLoudOnReplace(t *testing.T) {
	gw, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	cards := NewApprovalCards()
	gate := permissions.NewGate(permissions.NewRuleStore(""), nil)
	if err := gw.SetApprovalFallback(gate, 77, cards, "test"); err != nil {
		t.Fatalf("first SetApprovalFallback: %v", err)
	}
	if err := gw.SetApprovalFallback(gate, 77, cards, "test"); err == nil {
		t.Error("second approval fallback install accepted, want a loud error")
	}
}

func TestInstallCallbackRouterIdempotent(t *testing.T) {
	gw, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if err := gw.InstallCallbackRouter(); err != nil {
		t.Fatalf("first InstallCallbackRouter: %v", err)
	}
	// A second install is a no-op, not a silent replacement.
	if err := gw.InstallCallbackRouter(); err != nil {
		t.Errorf("second InstallCallbackRouter failed: %v (want a no-op success)", err)
	}
	if gw.callbackHandlerFor() == nil {
		t.Error("InstallCallbackRouter did not install a callback handler")
	}
	// But installing the router over a DIFFERENT handler fails
	// loudly (F-A20-6).
	gw2, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if err := gw2.SetCallbackHandler(func(ctx context.Context, q TGCallbackQuery) error { return nil }); err != nil {
		t.Fatalf("SetCallbackHandler: %v", err)
	}
	if err := gw2.InstallCallbackRouter(); err == nil {
		t.Error("InstallCallbackRouter over a custom handler accepted, want a loud error")
	}
}

func TestInstallApprovalCallbackHandlerLoudOnDuplicate(t *testing.T) {
	gw, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	cards := NewApprovalCards()
	gate := permissions.NewGate(permissions.NewRuleStore(""), nil)
	if err := InstallApprovalCallbackHandler(gw, gate, 77, cards, nil, "test"); err != nil {
		t.Fatalf("first InstallApprovalCallbackHandler: %v", err)
	}
	if gw.callbackHandlerFor() == nil {
		t.Fatal("InstallApprovalCallbackHandler did not install a callback handler")
	}
	// A second install on the same gateway fails loudly (F-A20-5):
	// the duplicate "ask:" registration used to be swallowed
	// with `_ =`, hiding a double-wiring bug.
	if err := InstallApprovalCallbackHandler(gw, gate, 77, cards, nil, "test"); err == nil {
		t.Error("second InstallApprovalCallbackHandler accepted, want a loud error")
	}
}
