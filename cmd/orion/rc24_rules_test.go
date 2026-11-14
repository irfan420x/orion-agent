// rc24 W-D (F-A18-10): /rules is served by the `orion` binary's
// daemonTurnHandler — the one slash command this binary dispatches
// (see newRulesCommandEnv) — so `orion telegram poll` and
// `orion daemon` answer /rules from the permission gate instead of
// passing it to the agent as plain text. The owner is recognised
// from a group chat (sender in ctx); a non-owner is refused; any
// other text still runs a turn.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/permissions"
)

func TestDaemonTurnHandlerRulesViaOrionBinary(t *testing.T) {
	const owner = 111
	const group = 222
	prov := &daemonScriptProvider{}
	a := daemonTestApp(t, prov)
	defer a.Close()
	a.Budget = budget.New(100000)
	a.Config.TelegramAllowedChats = []int64{owner}
	gate, err := a.NewPermissionGate(nil)
	if err != nil {
		t.Fatalf("NewPermissionGate: %v", err)
	}
	if gate == nil || gate.Rules() == nil {
		t.Fatal("NewPermissionGate returned no usable rule store")
	}
	if err := gate.Rules().Set("shell_exec", permissions.ActionAllow); err != nil {
		t.Fatalf("seed shell_exec rule: %v", err)
	}

	handler := daemonTurnHandler(a)

	// The owner lists the rules; no agent turn runs.
	reply, err := handler(context.Background(), owner, "/rules")
	if err != nil {
		t.Fatalf("owner /rules: err = %v", err)
	}
	if !strings.Contains(reply, "shell_exec") {
		t.Errorf("owner /rules = %q, want the shell_exec rule listed", reply)
	}
	if got := prov.Calls(); got != 0 {
		t.Errorf("provider calls after /rules = %d, want 0 (a command must not run a turn)", got)
	}

	// The owner sending from a group chat is recognised (F-A18-10).
	groupOwnerCtx := messaging.WithSenderID(context.Background(), owner)
	reply, err = handler(groupOwnerCtx, group, "/rules")
	if err != nil {
		t.Fatalf("owner-in-group /rules: err = %v", err)
	}
	if strings.Contains(reply, "owner-only") {
		t.Errorf("owner-in-group /rules = %q, want the rule list, not a refusal", reply)
	}
	if !strings.Contains(reply, "shell_exec") {
		t.Errorf("owner-in-group /rules = %q, want the shell_exec rule listed", reply)
	}

	// The owner in the group can delete a rule, too.
	reply, err = handler(groupOwnerCtx, group, "/rules delete shell_exec")
	if err != nil {
		t.Fatalf("owner-in-group /rules delete: err = %v", err)
	}
	if strings.Contains(reply, "owner-only") {
		t.Errorf("owner-in-group /rules delete = %q, want the deletion confirmation", reply)
	}
	if _, ok := gate.Rules().Lookup("shell_exec"); ok {
		t.Error("delete: shell_exec rule still present after the owner deleted it from a group chat")
	}

	// A non-owner chat is refused, and a non-owner member of the
	// same group is refused too.
	reply, err = handler(context.Background(), 999, "/rules")
	if err != nil {
		t.Fatalf("non-owner /rules: err = %v", err)
	}
	if !strings.Contains(reply, "owner-only") {
		t.Errorf("non-owner /rules = %q, want an owner-only refusal", reply)
	}
	reply, err = handler(messaging.WithSenderID(context.Background(), 555), group, "/rules")
	if err != nil {
		t.Fatalf("non-owner group member /rules: err = %v", err)
	}
	if !strings.Contains(reply, "owner-only") {
		t.Errorf("non-owner group member /rules = %q, want an owner-only refusal", reply)
	}

	// Anything else still runs a turn: only /rules is intercepted.
	reply, err = handler(context.Background(), owner, "hi")
	if err != nil {
		t.Fatalf("plain text turn: err = %v", err)
	}
	if reply != "ok" {
		t.Errorf("plain text turn reply = %q, want %q", reply, "ok")
	}
	if got := prov.Calls(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (only the plain-text turn runs the agent)", got)
	}
}
