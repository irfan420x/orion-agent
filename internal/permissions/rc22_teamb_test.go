package permissions

// rc22 Team B (F-A16-3): Always/Never rules are per-chat.
//
// Before rc22, RuleStore was keyed by tool name only, so "Always
// allow shell_exec" tapped in one chat approved the tool in every
// chat, including group chats — per-chat isolation held for
// history/model/budget but broke at the permission layer. These
// tests pin the fix: a rule set in chat A never affects chat B,
// an Always in one chat still requires approval in another, the
// denylist stays absolute in every chat, and a version 1 rules
// file (tool name only, no chat recorded) still loads — as a
// legacy global rule, so an existing decision is never silently
// dropped.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ctxForChat(chatID, senderID int64) context.Context {
	return WithApprovalContext(context.Background(), chatID, senderID)
}

// TestRC22PerChatAllowRuleIsolated: an allow rule for chat A lets
// chat A run the tool without prompting and leaves chat B
// prompting as before.
func TestRC22PerChatAllowRuleIsolated(t *testing.T) {
	rules := NewRuleStore("")
	if err := rules.SetForChat(111, "shell_exec", ActionAllow); err != nil {
		t.Fatal(err)
	}
	if a, ok := rules.LookupForChat(111, "shell_exec"); !ok || a != ActionAllow {
		t.Errorf("chat 111 shell_exec = %q/%v, want allow/true", a, ok)
	}
	if _, ok := rules.LookupForChat(222, "shell_exec"); ok {
		t.Error("chat 111 allow rule leaked into chat 222")
	}
	if _, ok := rules.Lookup("shell_exec"); ok {
		t.Error("chat 111 allow rule leaked into the global rules")
	}

	prompted := map[int64]bool{}
	g := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted[req.ChatID] = true
		return nil
	})
	g.SetTimeout(20 * time.Millisecond)
	if ok, reason := g.Check(ctxForChat(111, 111), "shell_exec", `{"command":"echo hi"}`); !ok {
		t.Errorf("chat 111 denied by its own allow rule: %s", reason)
	}
	if prompted[111] {
		t.Error("chat 111 was prompted despite its allow rule")
	}
	// Chat 222 has no rule: it must be prompted, then time out
	// (nobody answers) and be denied.
	if ok, _ := g.Check(ctxForChat(222, 222), "shell_exec", `{"command":"echo hi"}`); ok {
		t.Error("chat 222 allowed by chat 111's rule")
	}
	if !prompted[222] {
		t.Error("chat 222 was not prompted despite having no rule")
	}
}

// TestRC22PerChatDenyRuleIsolated: a deny rule for chat A refuses
// chat A without prompting and leaves chat B unaffected.
func TestRC22PerChatDenyRuleIsolated(t *testing.T) {
	rules := NewRuleStore("")
	if err := rules.SetForChat(111, "file_write", ActionDeny); err != nil {
		t.Fatal(err)
	}
	prompted := false
	g := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	if ok, _ := g.Check(ctxForChat(111, 111), "file_write", `{"path":"/tmp/x","content":"y"}`); ok {
		t.Error("chat 111 allowed despite its deny rule")
	}
	if prompted {
		t.Error("chat 111 was prompted despite its deny rule")
	}
	// Chat 222: no rule, so the gate must prompt (deny rule in
	// another chat is not its business). Use a prompter that
	// approves at once so the check returns promptly.
	var g2 *Gate
	g2 = NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		go func() { _ = g2.Resolve(req.ID, true, nil) }()
		return nil
	})
	if ok, reason := g2.Check(ctxForChat(222, 222), "file_write", `{"path":"/tmp/x","content":"y"}`); !ok {
		t.Errorf("chat 222 denied by chat 111's deny rule: %s", reason)
	}
}

// TestRC22AlwaysInOneChatStillPromptsInAnother proves the
// end-to-end behaviour the audit describes: tapping "Always
// allow" in chat A persists a rule for chat A only, so a later
// call in chat B still prompts.
func TestRC22AlwaysInOneChatStillPromptsInAnother(t *testing.T) {
	rules := NewRuleStore("")
	g := NewGate(rules, nil)
	allow := ActionAllow
	g.prompter = approvingPrompter(g, &allow)
	if ok, reason := g.Check(ctxForChat(111, 111), "web_fetch", `{"url":"https://example.com"}`); !ok {
		t.Fatalf("chat 111 always-allow call denied: %s", reason)
	}
	if a, ok := rules.LookupForChat(111, "web_fetch"); !ok || a != ActionAllow {
		t.Errorf("chat 111 rule = %q/%v, want allow/true", a, ok)
	}

	// Chat 111 now passes without prompting.
	prompted := false
	g.prompter = func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	}
	if ok, _ := g.Check(ctxForChat(111, 111), "web_fetch", `{"url":"https://example.com"}`); !ok {
		t.Error("chat 111 denied after its always-allow")
	}
	if prompted {
		t.Error("chat 111 prompted after its always-allow")
	}
	// Chat 222 still prompts.
	prompted = false
	g.prompter = func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		// Answer at once (deny), so the check returns promptly
		// instead of waiting out the 90s approval window.
		go func() { _ = g.Resolve(req.ID, false, nil) }()
		return nil
	}
	if ok, _ := g.Check(ctxForChat(222, 222), "web_fetch", `{"url":"https://example.com"}`); ok {
		t.Error("chat 222 allowed by chat 111's always-allow")
	}
	if !prompted {
		t.Error("chat 222 was not prompted despite having no rule")
	}
}

// TestRC22LegacyV1RulesLoadAsGlobal: the live daemon's
// permissions.json (version 1, {"shell_exec": "allow"}) must keep
// working — it loads as a global rule that still applies to every
// chat, because no chat was recorded when it was written. A
// per-chat rule set afterwards coexists with it, and saving writes
// version 2 without losing the legacy entry.
func TestRC22LegacyV1RulesLoadAsGlobal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	// Byte-for-byte the version 1 shape (as written by rc21).
	if err := os.WriteFile(path, []byte("{\n  \"version\": 1,\n  \"rules\": {\n    \"shell_exec\": \"allow\"\n  }\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewRuleStore(path)
	if err := s.LoadError(); err != nil {
		t.Fatalf("LoadError for a version 1 file: %v", err)
	}
	for _, chat := range []int64{0, 111, 222, -999} {
		if a, ok := s.LookupForChat(chat, "shell_exec"); !ok || a != ActionAllow {
			t.Errorf("legacy rule for chat %d = %q/%v, want allow/true", chat, a, ok)
		}
	}
	listed := s.List()
	if len(listed) != 1 || listed[0].ChatID != 0 || listed[0].Tool != "shell_exec" || listed[0].Action != ActionAllow {
		t.Errorf("List() = %+v, want one global shell_exec allow rule", listed)
	}
	// A per-chat deny in chat 222 must win over the legacy global
	// allow for that chat only (the chat's own rule is consulted
	// first); other chats keep the legacy allow.
	if err := s.SetForChat(222, "shell_exec", ActionDeny); err != nil {
		t.Fatal(err)
	}
	if a, ok := s.LookupForChat(222, "shell_exec"); !ok || a != ActionDeny {
		t.Errorf("chat 222 rule = %q/%v, want deny/true", a, ok)
	}
	if a, ok := s.LookupForChat(111, "shell_exec"); !ok || a != ActionAllow {
		t.Errorf("chat 111 rule = %q/%v, want allow/true (legacy global)", a, ok)
	}
	// Reload: both entries survive, and the file is now version 2.
	reloaded := NewRuleStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload LoadError: %v", err)
	}
	if a, ok := reloaded.LookupForChat(222, "shell_exec"); !ok || a != ActionDeny {
		t.Errorf("reloaded chat 222 rule = %q/%v, want deny/true", a, ok)
	}
	if a, ok := reloaded.LookupForChat(111, "shell_exec"); !ok || a != ActionAllow {
		t.Errorf("reloaded chat 111 rule = %q/%v, want allow/true", a, ok)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 2`) {
		t.Errorf("saved file is not version 2:\n%s", data)
	}
	if !strings.Contains(string(data), `"222:shell_exec"`) {
		t.Errorf("saved file has no per-chat key:\n%s", data)
	}
	if !strings.Contains(string(data), `"shell_exec": "allow"`) {
		t.Errorf("saved file lost the legacy global rule:\n%s", data)
	}
}

// TestRC22PerChatRuleRoundTrip: a per-chat rule persists to disk
// and reloads scoped to its chat; deleting it in one chat leaves
// the other chat's rule alone.
func TestRC22PerChatRuleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	s := NewRuleStore(path)
	if err := s.SetForChat(-100, "web_search", ActionDeny); err != nil {
		t.Fatal(err)
	}
	if err := s.SetForChat(111, "web_search", ActionAllow); err != nil {
		t.Fatal(err)
	}
	reloaded := NewRuleStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload LoadError: %v", err)
	}
	if a, ok := reloaded.LookupForChat(-100, "web_search"); !ok || a != ActionDeny {
		t.Errorf("reloaded group chat rule = %q/%v, want deny/true", a, ok)
	}
	if a, ok := reloaded.LookupForChat(111, "web_search"); !ok || a != ActionAllow {
		t.Errorf("reloaded chat 111 rule = %q/%v, want allow/true", a, ok)
	}
	if err := reloaded.DeleteForChat(111, "web_search"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.LookupForChat(111, "web_search"); ok {
		t.Error("chat 111 rule still present after DeleteForChat")
	}
	if a, ok := reloaded.LookupForChat(-100, "web_search"); !ok || a != ActionDeny {
		t.Errorf("group chat rule = %q/%v after deleting chat 111's rule, want deny/true", a, ok)
	}
	// Deleting a chat's rule never touches a global rule either.
	if err := reloaded.Set("shell_exec", ActionAllow); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.DeleteForChat(111, "shell_exec"); err != nil {
		t.Fatal(err)
	}
	if a, ok := reloaded.Lookup("shell_exec"); !ok || a != ActionAllow {
		t.Errorf("global rule = %q/%v after DeleteForChat, want allow/true", a, ok)
	}
}

// TestRC22DenylistStillAbsolutePerChat: a per-chat allow rule for
// shell_exec must not rescue a denylisted command in that chat —
// or in any other chat.
func TestRC22DenylistStillAbsolutePerChat(t *testing.T) {
	rules := NewRuleStore("")
	if err := rules.SetForChat(111, "shell_exec", ActionAllow); err != nil {
		t.Fatal(err)
	}
	if err := rules.SetForChat(222, "shell_exec", ActionAllow); err != nil {
		t.Fatal(err)
	}
	prompted := false
	g := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	for _, chat := range []int64{111, 222} {
		if ok, _ := g.Check(ctxForChat(chat, chat), "shell_exec", `{"command":"rm -rf /"}`); ok {
			t.Errorf("chat %d: denylisted command allowed by a per-chat allow rule", chat)
		}
	}
	if prompted {
		t.Error("denylisted command prompted the user")
	}
	// The same per-chat allow still permits a harmless command.
	if ok, reason := g.Check(ctxForChat(111, 111), "shell_exec", `{"command":"echo hi"}`); !ok {
		t.Errorf("harmless command denied by per-chat allow rule: %s", reason)
	}
}

// TestRC22RuleKeyEncoding: the on-disk key codec round-trips
// global and per-chat keys, including negative (group) chat IDs,
// and never mistakes a bare tool name for a per-chat key.
func TestRC22RuleKeyEncoding(t *testing.T) {
	cases := []ruleKey{
		{chatID: 0, tool: "shell_exec"},
		{chatID: 111, tool: "shell_exec"},
		{chatID: -1001234567890, tool: "web_fetch"},
		{chatID: 0, tool: "mcp__fs__write"},
	}
	for _, want := range cases {
		if got := decodeRuleKey(encodeRuleKey(want)); got != want {
			t.Errorf("decode(encode(%+v)) = %+v", want, got)
		}
	}
	// A bare tool name decodes as global, even when it looks odd.
	if got := decodeRuleKey("shell_exec"); got.chatID != 0 || got.tool != "shell_exec" {
		t.Errorf("decodeRuleKey(%q) = %+v, want global shell_exec", "shell_exec", got)
	}
}
