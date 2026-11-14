// W4 (rc12) regression tests for Audit 7 F-A7-P2-2: oriond was not
// on the shared gate constructor — its buildApprovalGate called
// permissions.NewGate directly — and the wireGate comment claiming
// otherwise was false. oriond now builds the gate through the
// shared constructor (app.NewPermissionGate) with the shared
// Telegram prompter/expiry hook from internal/messaging. This test
// proves oriond's side installs a non-nil gate on the app AND a
// working callback handler (proven through PollStream, not by
// calling the handler directly): a tap delivered by getUpdates must
// resolve a pending approval. The `orion daemon` / `orion telegram
// poll` side is proven the same way in
// cmd/orion/rc12_w4_gate_test.go; the shared wiring itself is
// proven in internal/messaging/approval_gate_test.go.
package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// TestW4BuildApprovalGateInstallsGateAndCallbackHandler proves
// buildApprovalGate installs a non-nil gate on the app (through
// the shared constructor) and that the callback handler installed
// alongside it resolves taps delivered through PollStream. A gate
// without an installed handler would prompt forever: the card goes
// out, the tap arrives, and nothing resolves it.
func TestW4BuildApprovalGateInstallsGateAndCallbackHandler(t *testing.T) {
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
	if gate == nil {
		t.Fatal("buildApprovalGate returned a nil gate — the daemon would run ungated")
	}
	if a.PermissionGate() != gate {
		t.Fatal("buildApprovalGate did not install the gate on the app (PermissionGate() != returned gate)")
	}
	if cards == nil {
		t.Fatal("buildApprovalGate returned a nil card tracker — expiry could never edit a card in place")
	}
	messaging.InstallApprovalCallbackHandler(gw, gate, ownerID, cards, nil, "oriond")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pollErr := make(chan error, 1)
	go func() {
		pollErr <- gw.PollStream(ctx, func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
			return "ok", nil, nil
		})
	}()

	checkCtx := permissions.WithApprovalContext(context.Background(), ownerID, ownerID)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := gate.Check(checkCtx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	approvalCard := func() (string, int64) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, s := range fake.sent {
			if s.ChatID != ownerID || !strings.Contains(s.Keyboard, "perm:approve:") {
				continue
			}
			var kb messaging.InlineKeyboardMarkup
			if err := json.Unmarshal([]byte(s.Keyboard), &kb); err != nil || len(kb.InlineKeyboard) == 0 || len(kb.InlineKeyboard[0]) == 0 {
				continue
			}
			if _, id, ok := messaging.ParsePermissionCallback(kb.InlineKeyboard[0][0].CallbackData); ok {
				return id, s.MessageID
			}
		}
		return "", 0
	}
	var reqID string
	var cardMsgID int64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		reqID, cardMsgID = approvalCard()
		if reqID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reqID == "" {
		t.Fatal("timed out waiting for the approval card")
	}
	fake.enqueue(messaging.Update{UpdateID: 1, CallbackQuery: &messaging.TGCallbackQuery{
		ID:      "cb-w4-approve",
		From:    &messaging.TGUser{ID: ownerID},
		Message: &messaging.TGMessage{MessageID: cardMsgID, Chat: messaging.TGChat{ID: ownerID}},
		Data:    "perm:approve:" + reqID,
	}})
	select {
	case ok := <-resultCh:
		if !ok {
			t.Error("Check returned false after the approve tap delivered through PollStream, want true")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Check did not return after the approve tap delivered through PollStream — buildApprovalGate installed no working callback handler")
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
