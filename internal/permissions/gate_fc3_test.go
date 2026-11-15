package permissions

import (
	"context"
	"strings"
	"testing"
	"time"
)

// --- Unattended (scheduled) turns (F-C3) ---------------------------------
//
// A scheduled turn runs with nobody present, so the gate must never
// auto-allow through a persisted "always allow" rule and must never
// wait the 90s approval window on an answer that never comes:
// prompt-or-deny only, never auto-allow. Interactive turns are
// completely unaffected: the same rule still auto-allows there.

// unattendedCtx is ctxWithChat's chat marked as a scheduled turn.
func unattendedCtx(t *testing.T) context.Context {
	t.Helper()
	return WithUnattendedContext(ctxWithChat(t))
}

// TestGateUnattendedIgnoresAllowRule is the F-C3 core case: the chat
// HAS a persisted "always allow" for shell_exec, but the unattended
// turn denies the call anyway — promptly, without ever calling the
// prompter (no card at 3am, no 90s wait).
func TestGateUnattendedIgnoresAllowRule(t *testing.T) {
	rules := NewRuleStore("")
	if err := rules.SetForChat(42, "shell_exec", ActionAllow); err != nil {
		t.Fatal(err)
	}
	prompted := false
	gate := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	start := time.Now()
	allowed, reason := gate.Check(unattendedCtx(t), "shell_exec", `{"command":"echo hi"}`)
	elapsed := time.Since(start)
	if allowed {
		t.Error("unattended Check(shell_exec) allowed the call, want denied (persisted allow rules never apply to scheduled turns)")
	}
	if !strings.Contains(reason, "unattended") {
		t.Errorf("unattended denial reason = %q, want it to name the unattended turn", reason)
	}
	if prompted {
		t.Error("prompter was called in an unattended turn, want no prompt card sent")
	}
	if elapsed > 10*time.Second {
		t.Errorf("unattended Check took %v, want well under the 90s approval window (it must deny promptly, not wait)", elapsed)
	}
}

// TestGateInteractiveStillHonoursAllowRule is the other side of the
// same rule: the identical allow rule in an interactive turn still
// auto-allows, with no prompt — the fix must not change interactive
// behaviour at all.
func TestGateInteractiveStillHonoursAllowRule(t *testing.T) {
	rules := NewRuleStore("")
	if err := rules.SetForChat(42, "shell_exec", ActionAllow); err != nil {
		t.Fatal(err)
	}
	prompted := false
	gate := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	if allowed, reason := gate.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`); !allowed {
		t.Errorf("interactive Check(shell_exec) = denied (%q), want allowed by the persisted allow rule", reason)
	}
	if prompted {
		t.Error("prompter was called for an allow-ruled tool in an interactive turn, want no prompt")
	}
}

// TestGateUnattendedDenyRuleStillDenies: a persisted DENY rule still
// denies in an unattended turn, promptly and without prompting —
// ignoring it too would only ever prompt, and an unattended turn
// never prompts.
func TestGateUnattendedDenyRuleStillDenies(t *testing.T) {
	rules := NewRuleStore("")
	if err := rules.SetForChat(42, "shell_exec", ActionDeny); err != nil {
		t.Fatal(err)
	}
	prompted := false
	gate := NewGate(rules, func(ctx context.Context, req ApprovalRequest) error {
		prompted = true
		return nil
	})
	if allowed, reason := gate.Check(unattendedCtx(t), "shell_exec", `{"command":"echo hi"}`); allowed {
		t.Error("unattended Check(shell_exec) allowed the call, want denied by the persisted deny rule")
	} else if !strings.Contains(reason, "denied by permission rule") {
		t.Errorf("unattended denial reason = %q, want the deny-rule reason", reason)
	}
	if prompted {
		t.Error("prompter was called for a deny-ruled tool, want no prompt")
	}
}

// TestGateUnattendedSafeToolStillAllowed: safe tools run freely in an
// unattended turn, exactly as interactively — the unattended denial
// applies to gated tools only.
func TestGateUnattendedSafeToolStillAllowed(t *testing.T) {
	gate := NewGate(NewRuleStore(""), nil)
	if allowed, reason := gate.Check(unattendedCtx(t), "calculator", `{"expression":"1+1"}`); !allowed {
		t.Errorf("unattended Check(calculator) = denied (%q), want allowed (safe tools run freely)", reason)
	}
}

// TestGateUnattendedDoesNotWaitForApproval: with NO rule at all, an
// unattended gated call is still denied promptly instead of
// prompting and waiting the full 90s window. The prompter returns
// nil and never resolves — if the gate called it, Check would hang
// until the 90s timeout — so the 5s watchdog bounds the test even
// on a regression, while the passing path returns in milliseconds.
func TestGateUnattendedDoesNotWaitForApproval(t *testing.T) {
	prompted := make(chan struct{}, 1)
	gate := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		prompted <- struct{}{}
		return nil
	})
	type result struct {
		allowed bool
		reason  string
	}
	done := make(chan result, 1)
	go func() {
		allowed, reason := gate.Check(unattendedCtx(t), "shell_exec", `{"command":"echo hi"}`)
		done <- result{allowed, reason}
	}()
	select {
	case r := <-done:
		if r.allowed {
			t.Error("unattended Check(shell_exec) allowed the call, want denied")
		}
		if !strings.Contains(r.reason, "unattended") {
			t.Errorf("unattended denial reason = %q, want it to name the unattended turn", r.reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unattended Check did not return within 5s — it must deny promptly, not wait the 90s approval window")
	}
	select {
	case <-prompted:
		t.Error("prompter was called in an unattended turn, want no prompt card sent")
	default:
	}
}

// TestUnattendedContextRoundTrip pins the marker itself: only a
// context marked by WithUnattendedContext reports unattended, and
// the marker survives alongside the approval context.
func TestUnattendedContextRoundTrip(t *testing.T) {
	if UnattendedFrom(nil) {
		t.Error("UnattendedFrom(nil) = true, want false")
	}
	if UnattendedFrom(context.Background()) {
		t.Error("UnattendedFrom(background) = true, want false")
	}
	ctx := WithUnattendedContext(WithApprovalContext(context.Background(), 42, 7))
	if !UnattendedFrom(ctx) {
		t.Error("UnattendedFrom(marked ctx) = false, want true")
	}
	if chat, sender, ok := ApprovalContextFrom(ctx); !ok || chat != 42 || sender != 7 {
		t.Errorf("ApprovalContextFrom(marked ctx) = (%d, %d, %v), want (42, 7, true)", chat, sender, ok)
	}
}
