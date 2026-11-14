package messaging

import (
	"context"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/permissions"
)

// rc23W3RulesEnv builds a stub CommandEnv for /rules backed by a
// real (in-memory) RuleStore, with owner as the owner chat.
func rc23W3RulesEnv(store *permissions.RuleStore, owner int64) *CommandEnv {
	return &CommandEnv{
		ChatID:     owner,
		OwnerID:    owner,
		IsOwner:    func(id int64) bool { return id == owner },
		ListRules:  store.List,
		DeleteRule: store.DeleteForChat,
		SetRule:    store.SetForChat,
	}
}

func rc23W3DispatchRules(t *testing.T, env *CommandEnv, text string) string {
	t.Helper()
	r := DefaultRegistry(env)
	reply, handled, err := r.Dispatch(context.Background(), env, text)
	if err != nil {
		t.Fatalf("Dispatch(%q): unexpected error %v", text, err)
	}
	if !handled {
		t.Fatalf("Dispatch(%q): not handled", text)
	}
	return reply
}

// TestRC23W3RulesCommand (F-A17-6): the owner-only /rules command
// lists legacy global rules (flagged as such), deletes a global or
// per-chat rule, and scopes a global rule onto one chat. A
// non-owner is refused; an unwired env reports unavailability.
func TestRC23W3RulesCommand(t *testing.T) {
	const owner = 7429947930
	store := permissions.NewRuleStore("")
	if err := store.Set("shell_exec", permissions.ActionAllow); err != nil {
		t.Fatalf("Set global rule: %v", err)
	}
	if err := store.SetForChat(111, "web_fetch", permissions.ActionDeny); err != nil {
		t.Fatalf("SetForChat: %v", err)
	}
	env := rc23W3RulesEnv(store, owner)

	reply := rc23W3DispatchRules(t, env, "/rules")
	if !strings.Contains(reply, "shell_exec") || !strings.Contains(reply, "GLOBAL (legacy") {
		t.Errorf("/rules list = %q, want the global shell_exec rule flagged legacy", reply)
	}
	if !strings.Contains(reply, "web_fetch") || !strings.Contains(reply, "chat 111") {
		t.Errorf("/rules list = %q, want the per-chat web_fetch rule for chat 111", reply)
	}

	// Scope the global rule onto one chat: global goes away, the
	// chat keeps it, other chats lose it.
	reply = rc23W3DispatchRules(t, env, "/rules scope shell_exec 222")
	if !strings.Contains(reply, "chat 222") {
		t.Errorf("/rules scope = %q, want confirmation for chat 222", reply)
	}
	if _, ok := store.Lookup("shell_exec"); ok {
		t.Error("scope: legacy global rule still present after scoping")
	}
	if a, ok := store.LookupForChat(222, "shell_exec"); !ok || a != permissions.ActionAllow {
		t.Errorf("scope: LookupForChat(222, shell_exec) = %q, %v, want allow, true", a, ok)
	}
	if _, ok := store.LookupForChat(333, "shell_exec"); ok {
		t.Error("scope: rule still applies to chat 333 after scoping")
	}

	// Delete a per-chat rule by <chat>:<tool>.
	reply = rc23W3DispatchRules(t, env, "/rules delete 111:web_fetch")
	if !strings.Contains(reply, "chat 111") {
		t.Errorf("/rules delete per-chat = %q, want confirmation for chat 111", reply)
	}
	if _, ok := store.LookupForChat(111, "web_fetch"); ok {
		t.Error("delete: per-chat web_fetch rule still present")
	}

	// Delete a legacy global rule.
	if err := store.Set("web_search", permissions.ActionAllow); err != nil {
		t.Fatalf("Set global web_search rule: %v", err)
	}
	reply = rc23W3DispatchRules(t, env, "/rules delete web_search")
	if !strings.Contains(reply, "legacy global rule") {
		t.Errorf("/rules delete global = %q, want the legacy-global confirmation", reply)
	}
	if _, ok := store.Lookup("web_search"); ok {
		t.Error("delete: global web_search rule still present")
	}

	// Scope with no global rule, and bad argument shapes, are
	// usage replies, not errors.
	if reply := rc23W3DispatchRules(t, env, "/rules scope nosuchtool 222"); !strings.Contains(reply, "No legacy global rule") {
		t.Errorf("/rules scope missing = %q, want a no-such-rule reply", reply)
	}
	if reply := rc23W3DispatchRules(t, env, "/rules delete"); !strings.Contains(reply, "Usage:") {
		t.Errorf("/rules delete (no arg) = %q, want usage", reply)
	}
	if reply := rc23W3DispatchRules(t, env, "/rules frobnicate"); !strings.Contains(reply, "Usage:") {
		t.Errorf("/rules frobnicate = %q, want usage", reply)
	}
}

func TestRC23W3RulesCommandOwnerOnly(t *testing.T) {
	const owner = 7429947930
	store := permissions.NewRuleStore("")
	env := rc23W3RulesEnv(store, owner)

	// A non-owner chat is refused and nothing changes.
	env.ChatID = 999
	reply := rc23W3DispatchRules(t, env, "/rules")
	if !strings.Contains(reply, "owner-only") {
		t.Errorf("/rules as non-owner = %q, want an owner-only refusal", reply)
	}
	reply = rc23W3DispatchRules(t, env, "/rules delete shell_exec")
	if !strings.Contains(reply, "owner-only") {
		t.Errorf("/rules delete as non-owner = %q, want an owner-only refusal", reply)
	}

	// An unwired env (nil IsOwner) reports unavailability, never
	// guesses at ownership.
	env.ChatID = owner
	env.IsOwner = nil
	reply = rc23W3DispatchRules(t, env, "/rules")
	if !strings.Contains(reply, "unavailable") {
		t.Errorf("/rules unwired = %q, want an unavailable reply", reply)
	}
}
