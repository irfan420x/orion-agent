package permissions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func ctxWithChat(t *testing.T) context.Context {
	t.Helper()
	return WithApprovalContext(context.Background(), 42, 42)
}

func TestClassify(t *testing.T) {
	cases := map[string]Class{
		"shell_exec":     ClassDestructive,
		"file_write":     ClassDestructive,
		"file_edit":      ClassDestructive,
		"subagent_spawn": ClassDestructive,
		"web_fetch":      ClassNetwork,
		"web_search":     ClassNetwork,
		"calculator":     ClassSafe,
		"file_read":      ClassSafe,
		"subagent_list":  ClassSafe,
		"subagent_stop":  ClassSafe,
		"unknown_tool":   ClassSafe,
		"":               ClassSafe,
		// MCP tools (rc9): any server tool is ClassNetwork — the
		// server is an external process with unknown capabilities.
		"mcp__fake__echo": ClassNetwork,
		"mcp__fs__write":  ClassNetwork,
	}
	for tool, want := range cases {
		if got := Classify(tool); got != want {
			t.Errorf("Classify(%q) = %v, want %v", tool, got, want)
		}
	}
	if !IsGated("shell_exec") || IsGated("calculator") {
		t.Error("IsGated disagrees with Classify")
	}
}

func TestRuleStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	s := NewRuleStore(path)
	if err := s.LoadError(); err != nil {
		t.Fatalf("LoadError: %v", err)
	}
	if _, ok := s.Lookup("shell_exec"); ok {
		t.Fatal("fresh store has a shell_exec rule")
	}
	if err := s.Set("shell_exec", ActionDeny); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("web_fetch", ActionAllow); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("calculator", Action("maybe")); err == nil {
		t.Error("Set with invalid action: expected error")
	}
	if err := s.Set("", ActionAllow); err == nil {
		t.Error("Set with empty tool: expected error")
	}
	// 0600 on disk.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat rules file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("rules file mode = %o, want 0600", fi.Mode().Perm())
	}
	reloaded := NewRuleStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload LoadError: %v", err)
	}
	if a, ok := reloaded.Lookup("shell_exec"); !ok || a != ActionDeny {
		t.Errorf("reloaded shell_exec = %q/%v, want deny/true", a, ok)
	}
	if a, ok := reloaded.Lookup("web_fetch"); !ok || a != ActionAllow {
		t.Errorf("reloaded web_fetch = %q/%v, want allow/true", a, ok)
	}
	if err := reloaded.Delete("shell_exec"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := reloaded.Lookup("shell_exec"); ok {
		t.Error("rule still present after Delete")
	}
	// Corrupt file: empty rules + load error, never a grant.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := NewRuleStore(path)
	if err := bad.LoadError(); err == nil {
		t.Error("corrupt rules file: expected a load error")
	}
	if _, ok := bad.Lookup("web_fetch"); ok {
		t.Error("corrupt rules file: Lookup returned a rule")
	}
}

func TestGateNilAllowsEverything(t *testing.T) {
	var g *Gate
	if ok, _ := g.Check(ctxWithChat(t), "shell_exec", `{"command":"rm -rf /"}`); !ok {
		t.Error("nil gate must allow (no gate installed = CLI behaviour)")
	}
	if g.PendingCount() != 0 {
		t.Error("nil gate PendingCount != 0")
	}
	if g.Resolve("x", true, nil) {
		t.Error("nil gate Resolve returned true")
	}
}

func TestGateSafeToolsNeedNoApproval(t *testing.T) {
	prompted := false
	g := NewGate(nil, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	if ok, reason := g.Check(ctxWithChat(t), "calculator", `{"expression":"1+1"}`); !ok {
		t.Errorf("calculator denied: %s", reason)
	}
	if prompted {
		t.Error("prompter was called for a safe tool")
	}
}

func TestGateDenyRuleShortCircuits(t *testing.T) {
	rules := NewRuleStore("")
	if err := rules.Set("shell_exec", ActionDeny); err != nil {
		t.Fatal(err)
	}
	prompted := false
	g := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	ok, reason := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
	if ok {
		t.Error("deny rule allowed the call")
	}
	if reason == "" {
		t.Error("deny reason is empty")
	}
	if prompted {
		t.Error("deny rule prompted the user")
	}
}

func TestGateAllowRuleShortCircuits(t *testing.T) {
	rules := NewRuleStore("")
	if err := rules.Set("file_write", ActionAllow); err != nil {
		t.Fatal(err)
	}
	prompted := false
	g := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	if ok, reason := g.Check(ctxWithChat(t), "file_write", `{"path":"/tmp/x","content":"y"}`); !ok {
		t.Errorf("allow rule denied the call: %s", reason)
	}
	if prompted {
		t.Error("allow rule prompted the user")
	}
}

// approvingPrompter resolves the request itself, like the Telegram
// callback handler does after the user taps a button.
func approvingPrompter(g *Gate, persist *Action) Prompter {
	return func(ctx context.Context, req ApprovalRequest) error {
		go func() {
			// Give Check a moment to start waiting.
			time.Sleep(10 * time.Millisecond)
			g.Resolve(req.ID, true, persist)
		}()
		return nil
	}
}

func TestGateApproveRoundTrip(t *testing.T) {
	g := NewGate(NewRuleStore(""), nil)
	g.prompter = approvingPrompter(g, nil)
	ok, reason := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
	if !ok {
		t.Fatalf("approved call denied: %s", reason)
	}
	if g.PendingCount() != 0 {
		t.Error("pending request leaked after approval")
	}
}

func TestGateDenyRoundTrip(t *testing.T) {
	g := NewGate(NewRuleStore(""), nil)
	g.prompter = func(ctx context.Context, req ApprovalRequest) error {
		go func() {
			time.Sleep(10 * time.Millisecond)
			g.Resolve(req.ID, false, nil)
		}()
		return nil
	}
	ok, reason := g.Check(ctxWithChat(t), "file_write", `{"path":"/tmp/x","content":"y"}`)
	if ok {
		t.Error("declined call was allowed")
	}
	if reason == "" {
		t.Error("decline reason is empty")
	}
}

func TestGateAlwaysAllowPersistsRule(t *testing.T) {
	rules := NewRuleStore("")
	g := NewGate(rules, nil)
	allow := ActionAllow
	g.prompter = approvingPrompter(g, &allow)
	if ok, reason := g.Check(ctxWithChat(t), "web_fetch", `{"url":"https://example.com"}`); !ok {
		t.Fatalf("always-allow call denied: %s", reason)
	}
	// The rule is per-chat (F-A16-3): it is stored for chat 42
	// (the ctxWithChat chat), not globally.
	if a, ok := rules.LookupForChat(42, "web_fetch"); !ok || a != ActionAllow {
		t.Errorf("rule after always-allow = %q/%v, want allow/true", a, ok)
	}
	if _, ok := rules.LookupForChat(43, "web_fetch"); ok {
		t.Error("always-allow rule for chat 42 leaked into chat 43")
	}
	if _, ok := rules.Lookup("web_fetch"); ok {
		t.Error("always-allow rule for chat 42 leaked into the global rules")
	}
	// Second call now passes without prompting.
	prompted := false
	g.prompter = func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	}
	if ok, _ := g.Check(ctxWithChat(t), "web_fetch", `{"url":"https://example.com"}`); !ok {
		t.Error("persisted allow rule denied the call")
	}
	if prompted {
		t.Error("persisted allow rule prompted again")
	}
}

func TestGateNeverAllowPersistsDenyRule(t *testing.T) {
	rules := NewRuleStore("")
	g := NewGate(rules, nil)
	deny := ActionDeny
	g.prompter = func(ctx context.Context, req ApprovalRequest) error {
		go func() {
			time.Sleep(10 * time.Millisecond)
			g.Resolve(req.ID, false, &deny)
		}()
		return nil
	}
	if ok, _ := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`); ok {
		t.Fatal("never-allow call was allowed")
	}
	// Per-chat (F-A16-3): stored for chat 42 only.
	if a, ok := rules.LookupForChat(42, "shell_exec"); !ok || a != ActionDeny {
		t.Errorf("rule after never-allow = %q/%v, want deny/true", a, ok)
	}
	if _, ok := rules.LookupForChat(43, "shell_exec"); ok {
		t.Error("never-allow rule for chat 42 leaked into chat 43")
	}
}

func TestGateDenylistIsAbsolute(t *testing.T) {
	// The approver would approve anything — the denylist must still
	// win, and must win WITHOUT prompting (no request is ever
	// registered, so there is nothing to approve).
	prompted := false
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	// A destructive command from the denylist test corpus. The
	// value is a test string only; nothing is executed.
	for _, cmd := range []string{"rm -rf /", "rm -rf / --no-preserve-root", "mkfs.ext4 /dev/sda1", "iptables -L"} {
		ok, reason := g.Check(ctxWithChat(t), "shell_exec", `{"command":`+quoteJSON(cmd)+`}`)
		if ok {
			t.Errorf("denylisted command %q was allowed", cmd)
		}
		if reason == "" {
			t.Errorf("denylisted command %q: empty reason", cmd)
		}
	}
	if prompted {
		t.Error("denylisted command prompted the user (approval must never be offered)")
	}
	if g.PendingCount() != 0 {
		t.Error("denylisted command left a pending request")
	}
	// An "always allow" rule for shell_exec must NOT rescue a
	// denylisted command either: the gate checks the denylist
	// before consulting rules.
	prompted = false
	rules := NewRuleStore("")
	if err := rules.Set("shell_exec", ActionAllow); err != nil {
		t.Fatal(err)
	}
	g2 := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	ok, _ := g2.Check(ctxWithChat(t), "shell_exec", `{"command":"rm -rf /"}`)
	if ok {
		t.Error("allow rule rescued a denylisted command")
	}
	// ...but the same rule still allows a harmless command.
	ok, reason := g2.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
	if !ok {
		t.Errorf("allow rule denied a harmless command: %s", reason)
	}
	if prompted {
		t.Error("allow rule prompted the user")
	}
}

func quoteJSON(s string) string {
	// Minimal JSON string quoting for the test (no control chars in
	// the test commands).
	out := `"`
	for _, r := range s {
		if r == '"' || r == '\\' {
			out += `\`
		}
		out += string(r)
	}
	return out + `"`
}

func TestGateTimeoutDenies(t *testing.T) {
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		return nil // prompt "sent", nobody answers
	})
	g.SetTimeout(50 * time.Millisecond)
	start := time.Now()
	ok, reason := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
	if ok {
		t.Error("timed-out approval was allowed")
	}
	if reason == "" {
		t.Error("timeout reason is empty")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %v, far longer than the 50ms bound", elapsed)
	}
	if g.PendingCount() != 0 {
		t.Error("timed-out request leaked")
	}
}

// TestGateOnExpireFiresOnTimeoutNotOnResolve: the expiry hook (used
// by the daemon to expire the approval card in place — the rc8
// incident: a tap on an already-timed-out card looks exactly like a
// broken button) fires exactly once when the window elapses, and
// never when the user answers in time.
func TestGateOnExpireFiresOnTimeoutNotOnResolve(t *testing.T) {
	expired := make(chan ApprovalRequest, 1)

	// Case 1: nobody answers — the hook fires with the request.
	reqCh := make(chan ApprovalRequest, 1)
	g2prompter := func(ctx context.Context, req ApprovalRequest) error {
		reqCh <- req
		return nil
	}
	g2 := NewGate(NewRuleStore(""), g2prompter)
	g2.SetTimeout(50 * time.Millisecond)
	g2.SetOnExpire(func(req ApprovalRequest) { expired <- req })
	go func() {
		_, _ = g2.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
	}()
	var req ApprovalRequest
	select {
	case req = <-reqCh:
	case <-time.After(5 * time.Second):
		t.Fatal("gate never prompted")
	}
	select {
	case got := <-expired:
		if got.ID != req.ID {
			t.Errorf("OnExpire request id = %q, want %q", got.ID, req.ID)
		}
		if got.Tool != "shell_exec" || got.ChatID != 42 {
			t.Errorf("OnExpire request = %+v, want shell_exec for chat 42", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnExpire did not fire on timeout")
	}
	if g2.PendingCount() != 0 {
		t.Error("timed-out request leaked")
	}

	// Case 2: the user answers in time — the hook never fires.
	answered := make(chan bool, 1)
	var g3 *Gate
	g3 = NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		// Answer at once, as a tap would.
		go func() { _ = g3.Resolve(req.ID, true, nil) }()
		return nil
	})
	g3.SetOnExpire(func(req ApprovalRequest) { expired <- req })
	go func() {
		ok, _ := g3.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
		answered <- ok
	}()
	select {
	case ok := <-answered:
		if !ok {
			t.Error("answered approval was denied")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("answered Check did not return")
	}
	select {
	case got := <-expired:
		t.Errorf("OnExpire fired for an answered request (%q)", got.ID)
	case <-time.After(200 * time.Millisecond):
		// No hook: correct.
	}
}

func TestGateCancelledContextDenies(t *testing.T) {
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		return nil
	})
	ctx, cancel := context.WithCancel(ctxWithChat(t))
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	ok, reason := g.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
	if ok {
		t.Error("cancelled approval was allowed")
	}
	if reason == "" {
		t.Error("cancel reason is empty")
	}
}

func TestGateNoPrompterDeniesGated(t *testing.T) {
	g := NewGate(NewRuleStore(""), nil)
	if ok, reason := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`); ok {
		t.Error("no-prompter gate allowed a gated call")
	} else if reason == "" {
		t.Error("no-prompter deny reason is empty")
	}
	// Safe tools still pass with no prompter.
	if ok, _ := g.Check(ctxWithChat(t), "calculator", `{}`); !ok {
		t.Error("no-prompter gate denied a safe tool")
	}
}

func TestGatePrompterErrorDenies(t *testing.T) {
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		return errors.New("send failed")
	})
	if ok, reason := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`); ok {
		t.Error("prompter error allowed the call")
	} else if reason == "" {
		t.Error("prompter-error reason is empty")
	}
	if g.PendingCount() != 0 {
		t.Error("failed prompt left a pending request")
	}
}

func TestGateResolveUnknownReturnsFalse(t *testing.T) {
	g := NewGate(NewRuleStore(""), nil)
	if g.Resolve("no-such-id", true, nil) {
		t.Error("Resolve of an unknown id returned true")
	}
	if _, ok := g.Pending("no-such-id"); ok {
		t.Error("Pending of an unknown id returned true")
	}
}

func TestRequestSummary(t *testing.T) {
	s := RequestSummary("shell_exec", `{"command":"echo hello world"}`)
	if s == "" || !contains(s, "shell_exec") || !contains(s, "echo hello") {
		t.Errorf("shell summary = %q, want tool + command", s)
	}
	s = RequestSummary("file_write", `{"path":"/tmp/secret.txt","content":"x"}`)
	if !contains(s, "/tmp/secret.txt") {
		t.Errorf("file summary = %q, want the path", s)
	}
	// A secret in the arguments must not reach the prompt verbatim.
	s = RequestSummary("shell_exec", `{"command":"echo sk-testfakekey1234567890"}`)
	if contains(s, "sk-testfakekey1234567890") {
		t.Errorf("summary leaked a secret: %q", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestApprovalContextRoundTrip(t *testing.T) {
	ctx := WithApprovalContext(context.Background(), 111, 222)
	chat, sender, ok := ApprovalContextFrom(ctx)
	if !ok || chat != 111 || sender != 222 {
		t.Errorf("ApprovalContextFrom = (%d, %d, %v), want (111, 222, true)", chat, sender, ok)
	}
	if _, _, ok := ApprovalContextFrom(context.Background()); ok {
		t.Error("bare context reported an approval context")
	}
	if _, _, ok := ApprovalContextFrom(nil); ok {
		t.Error("nil context reported an approval context")
	}
}
