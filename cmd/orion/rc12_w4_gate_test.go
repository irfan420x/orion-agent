// W4 (rc12) regression tests for Audit 7 F-A7-P2-2: oriond was not
// on the shared gate constructor, and the wireGate comment claiming
// it was is false. Both binaries now build the gate through the
// shared constructor (app.NewPermissionGate) with the shared
// Telegram prompter/expiry hook/tap resolver from
// internal/messaging. This file proves the `orion daemon` /
// `orion telegram poll` side installs a non-nil gate AND a working
// callback handler (proven through PollStream, not by calling the
// handler directly): a tap delivered by getUpdates must resolve a
// pending approval. The oriond side is proven the same way in
// cmd/oriond/rc12_w4_gate_test.go; the shared wiring itself is
// proven in internal/messaging/approval_gate_test.go.
package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/permissions"
)

// TestW4WireGateInstallsGateAndCallbackHandler proves wireGate
// installs a non-nil gate on the app and a callback handler on the
// gateway that resolves taps delivered through PollStream. A gate
// without an installed handler would prompt forever: the card goes
// out, the tap arrives, and nothing resolves it.
func TestW4WireGateInstallsGateAndCallbackHandler(t *testing.T) {
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
	gate, _, err := wireGate(a, gw)
	if err != nil {
		t.Fatalf("wireGate: %v", err)
	}
	if gate == nil {
		t.Fatal("wireGate returned a nil gate — a Telegram loop built from it would run ungated")
	}
	if a.PermissionGate() != gate {
		t.Fatal("wireGate did not install the gate on the app (PermissionGate() != returned gate)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pollErr := make(chan error, 1)
	go func() {
		pollErr <- gw.PollStream(ctx, func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
			return "ok", nil, nil
		})
	}()

	checkCtx := permissions.WithApprovalContext(context.Background(), 111, 111)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := gate.Check(checkCtx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var reqID string
	var cardMsgID int64
	rc11WaitFor(t, "approval card for chat 111", func() bool {
		reqID, cardMsgID = bot.approvalCard(111)
		return reqID != ""
	})
	bot.enqueue(messaging.Update{UpdateID: 1, CallbackQuery: &messaging.TGCallbackQuery{
		ID:      "cb-w4-approve",
		From:    &messaging.TGUser{ID: 111},
		Message: &messaging.TGMessage{MessageID: cardMsgID, Chat: messaging.TGChat{ID: 111}},
		Data:    "perm:approve:" + reqID,
	}})
	select {
	case ok := <-resultCh:
		if !ok {
			t.Error("Check returned false after the approve tap delivered through PollStream, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the approve tap delivered through PollStream — wireGate installed no working callback handler")
	}
	cancel()
	select {
	case err := <-pollErr:
		if err != nil {
			t.Errorf("PollStream after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PollStream did not return after cancel")
	}
}
