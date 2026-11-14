// Case-folding tests for the permission gate (F-A22-6, F-A22-7's
// rule-store side): a tool name that differs from a mapped name
// only by case must be treated as that tool everywhere —
// denylist, rules, and persisted outcomes — never as a second,
// unguarded identity.
package permissions

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestNormalizeToolName pins the single normaliser: trim + fold to
// lower case. Every entry point in this package routes through it;
// a second, drifting spelling of the fold is a bug (F-A22-6).
func TestNormalizeToolName(t *testing.T) {
	for in, want := range map[string]string{
		"shell_exec":   "shell_exec",
		"SHELL_EXEC":   "shell_exec",
		"Shell_Exec":   "shell_exec",
		"  shell_exec": "shell_exec",
		"shell_exec  ": "shell_exec",
		"CALCULATOR":   "calculator",
		"":             "",
		"   ":          "",
	} {
		if got := NormalizeToolName(in); got != want {
			t.Errorf("NormalizeToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGateDenylistCaseVariant reproduces the Audit 22 F-A22-6 live
// probe: Check("SHELL_EXEC", {"command":"rm -rf /"}) must deny
// on the denylist — no prompt, reason naming the denylist —
// exactly like the lowercase control. Before the fix the
// case-variant call prompted (and only died on approval timeout),
// so approving it would have authorised at the gate layer what
// the denylist forbids forever.
func TestGateDenylistCaseVariant(t *testing.T) {
	denylisted := `{"command":"rm -rf /"}`
	for _, name := range []string{"SHELL_EXEC", "Shell_Exec", " shell_exec ", "sHeLl_ExEc"} {
		prompted := false
		g := NewGate(nil, func(ctx context.Context, req ApprovalRequest) error {
			prompted = true
			return nil
		})
		// A short timeout keeps the test fast if the gate ever
		// prompts again: the failure then is "prompted", not a
		// 90s hang.
		g.SetTimeout(50 * time.Millisecond)
		ok, reason := g.Check(ctxWithChat(t), name, denylisted)
		if ok {
			t.Errorf("Check(%q, denylisted): allowed, want denied", name)
		}
		if !strings.Contains(reason, "denylist") {
			t.Errorf("Check(%q, denylisted): reason %q does not mention the denylist", name, reason)
		}
		if prompted {
			t.Errorf("Check(%q, denylisted): prompter was called — a denylisted command must never reach the user as an approval question", name)
		}
	}
}

// TestGateDenylistCaseVariantControl is the negative control for
// the probe above: the lowercase spelling denies the same way,
// and a NON-denylisted command under a case-variant name still
// prompts (classification still works; the fix must not turn the
// denylist into a blanket deny for the tool).
func TestGateDenylistCaseVariantControl(t *testing.T) {
	prompted := false
	g := NewGate(nil, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	ok, reason := g.Check(ctxWithChat(t), "shell_exec", `{"command":"rm -rf /"}`)
	if ok || !strings.Contains(reason, "denylist") || prompted {
		t.Errorf("lowercase control: ok=%v reason=%q prompted=%v, want denied/denylist/no-prompt", ok, reason, prompted)
	}
	g2 := NewGate(nil, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	g2.SetTimeout(time.Millisecond) // prompt, then time out fast
	prompted = false
	ok, _ = g2.Check(ctxWithChat(t), "SHELL_EXEC", `{"command":"echo hi"}`)
	if ok {
		t.Error("non-denylisted SHELL_EXEC call was allowed without approval")
	}
	if !prompted {
		t.Error("non-denylisted SHELL_EXEC call never prompted: the fix must not deny the tool outright")
	}
}

// TestGateApplyResultDenylistCaseVariant pins the second
// exact-match site (F-A22-6, applyResult): an approval outcome
// for a denylisted command under a case-variant name must still
// be denied, and an "always allow" rule must never be persisted
// for it — even if a future refactor reorders Check.
func TestGateApplyResultDenylistCaseVariant(t *testing.T) {
	rules := NewRuleStore("")
	g := NewGate(rules, nil)
	allow := ActionAllow
	ok, reason := g.applyResult("SHELL_EXEC", `{"command":"rm -rf /"}`, 42, pendingResult{approved: true, persist: &allow})
	if ok {
		t.Error("applyResult(SHELL_EXEC, denylisted, approved+always-allow): allowed, want denied")
	}
	if !strings.Contains(reason, "denylist") {
		t.Errorf("applyResult reason %q does not mention the denylist", reason)
	}
	if _, found := rules.LookupForChat(42, "shell_exec"); found {
		t.Error("applyResult persisted an always-allow rule for a denylisted command")
	}
}

// TestRuleStoreCaseFoldsToolNames pins that the rule store itself
// is case-folded: a rule set under one spelling is found under
// any other, and deleted under any other. Otherwise a legacy
// "SHELL_EXEC" rule and a "shell_exec" lookup would silently
// disagree, and the gate's folded lookup would miss rules users
// already made.
func TestRuleStoreCaseFoldsToolNames(t *testing.T) {
	s := NewRuleStore("")
	if err := s.Set("SHELL_EXEC", ActionDeny); err != nil {
		t.Fatalf("Set(SHELL_EXEC): %v", err)
	}
	if a, ok := s.Lookup("shell_exec"); !ok || a != ActionDeny {
		t.Errorf("Lookup(shell_exec) after Set(SHELL_EXEC) = %q/%v, want deny/true", a, ok)
	}
	if a, ok := s.LookupForChat(42, "Shell_Exec"); !ok || a != ActionDeny {
		t.Errorf("LookupForChat(42, Shell_Exec) = %q/%v, want deny/true (legacy global fallback)", a, ok)
	}
	if err := s.SetForChat(42, "File_Write", ActionAllow); err != nil {
		t.Fatalf("SetForChat(42, File_Write): %v", err)
	}
	if a, ok := s.LookupForChat(42, "file_write"); !ok || a != ActionAllow {
		t.Errorf("LookupForChat(42, file_write) after SetForChat(File_Write) = %q/%v, want allow/true", a, ok)
	}
	if err := s.Delete("shell_exec"); err != nil {
		t.Fatalf("Delete(shell_exec): %v", err)
	}
	if _, ok := s.Lookup("SHELL_EXEC"); ok {
		t.Error("rule set as SHELL_EXEC still present after Delete(shell_exec)")
	}
}
