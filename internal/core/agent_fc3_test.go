package core

import (
	"context"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// --- Unattended (scheduled) turns (F-C3, F-C8) ---------------------------
//
// The agent-loop view of the fix: in an unattended turn a persisted
// "always allow" rule must not run the tool, the turn must not wait
// on a prompt, and the denial must be counted — TurnStats must show
// ToolCalls == GateDeniedCalls == 1 so the scheduler can count the
// turn as a job failure instead of "ok".

// fc3Turn runs one turn with prov's scripted provider and gate, and
// returns the reply and the TurnFinished event (which carries the
// per-turn tool counts).
func fc3Turn(t *testing.T, ctx context.Context, prov *fakeProvider, gate *permissions.Gate, stub *gateStubTool) (string, ProgressEvent) {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	a.SetPermissionGate(gate)
	var finished ProgressEvent
	gotFinished := false
	out, err := a.RunWithEventHandler(ctx, "run the shell", func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			finished = ev
			gotFinished = true
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !gotFinished {
		t.Fatal("no TurnFinished event emitted")
	}
	return out, finished
}

// TestUnattendedTurnIgnoresAllowRuleAndCountsDenial: the chat has a
// persisted "always allow" for shell_exec, but the unattended turn
// denies the call — the tool never runs, the loop continues to a
// final answer, no prompt card is ever sent, the turn returns in
// milliseconds (no 90s wait), and TurnStats counts the denial.
func TestUnattendedTurnIgnoresAllowRuleAndCountsDenial(t *testing.T) {
	rules := permissions.NewRuleStore("")
	if err := rules.SetForChat(42, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	prompted := false
	gate := permissions.NewGate(rules, func(ctx context.Context, req permissions.ApprovalRequest) error {
		prompted = true
		return nil
	})
	stub := &gateStubTool{name: "shell_exec"}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "shell_exec", `{"command":"echo hi"}`), nil
			}
			return textResponse("done"), nil
		},
	}
	ctx := permissions.WithUnattendedContext(permissions.WithApprovalContext(context.Background(), 42, 42))
	start := time.Now()
	out, finished := fc3Turn(t, ctx, prov, gate, stub)
	elapsed := time.Since(start)
	if out != "done" {
		t.Fatalf("Run = %q, want %q (the loop must continue after an unattended denial)", out, "done")
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("shell_exec executions = %d, want 0 (allow rule ignored in an unattended turn)", got)
	}
	if prompted {
		t.Error("prompter was called in an unattended turn, want no prompt card sent")
	}
	if elapsed > 10*time.Second {
		t.Errorf("unattended turn took %v, want well under the 90s approval window (it must deny promptly)", elapsed)
	}
	if finished.ToolCalls != 1 || finished.GateDeniedCalls != 1 {
		t.Errorf("TurnFinished ToolCalls/GateDeniedCalls = %d/%d, want 1/1 (the denied call must be counted for F-C8)", finished.ToolCalls, finished.GateDeniedCalls)
	}
}

// TestInteractiveTurnStillHonoursAllowRule is the control for the
// test above: the identical allow rule in an interactive turn still
// runs the tool, with no prompt and no denial counted.
func TestInteractiveTurnStillHonoursAllowRule(t *testing.T) {
	rules := permissions.NewRuleStore("")
	if err := rules.SetForChat(42, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	prompted := false
	gate := permissions.NewGate(rules, func(ctx context.Context, req permissions.ApprovalRequest) error {
		prompted = true
		return nil
	})
	stub := &gateStubTool{name: "shell_exec"}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "shell_exec", `{"command":"echo hi"}`), nil
			}
			return textResponse("done"), nil
		},
	}
	ctx := permissions.WithApprovalContext(context.Background(), 42, 42)
	out, finished := fc3Turn(t, ctx, prov, gate, stub)
	if out != "done" {
		t.Fatalf("Run = %q, want %q", out, "done")
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("shell_exec executions = %d, want 1 (interactive turn still honours the allow rule)", got)
	}
	if prompted {
		t.Error("prompter was called for an allow-ruled tool in an interactive turn, want no prompt")
	}
	if finished.ToolCalls != 1 || finished.GateDeniedCalls != 0 {
		t.Errorf("TurnFinished ToolCalls/GateDeniedCalls = %d/%d, want 1/0", finished.ToolCalls, finished.GateDeniedCalls)
	}
}

// TestTurnWithMixedCallsNotAllDenied: an unattended turn where one
// call is gate-denied and another (safe) call runs is NOT an
// all-denied turn — only ToolCalls == GateDeniedCalls counts toward
// the job's failure accounting (F-C8), a partially successful turn
// records "ok".
func TestTurnWithMixedCallsNotAllDenied(t *testing.T) {
	rules := permissions.NewRuleStore("")
	if err := rules.SetForChat(42, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	gate := permissions.NewGate(rules, nil)
	shellStub := &gateStubTool{name: "shell_exec"}
	readStub := &gateStubTool{name: "file_read"}
	reg := tools.NewRegistry()
	for _, stub := range []*gateStubTool{shellStub, readStub} {
		if err := reg.Register(stub); err != nil {
			t.Fatal(err)
		}
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			switch call {
			case 1:
				return toolCallResponse("call-1", "shell_exec", `{"command":"echo hi"}`), nil
			case 2:
				return toolCallResponse("call-2", "file_read", `{"path":"/tmp/x"}`), nil
			default:
				return textResponse("done"), nil
			}
		},
	}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	a.SetPermissionGate(gate)
	ctx := permissions.WithUnattendedContext(permissions.WithApprovalContext(context.Background(), 42, 42))
	var finished ProgressEvent
	out, err := a.RunWithEventHandler(ctx, "do the thing", func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			finished = ev
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Fatalf("Run = %q, want %q", out, "done")
	}
	if got := shellStub.callCount(); got != 0 {
		t.Errorf("shell_exec executions = %d, want 0 (allow rule ignored unattended)", got)
	}
	if got := readStub.callCount(); got != 1 {
		t.Errorf("file_read executions = %d, want 1 (safe tools still run in an unattended turn)", got)
	}
	if finished.ToolCalls != 2 || finished.GateDeniedCalls != 1 {
		t.Errorf("TurnFinished ToolCalls/GateDeniedCalls = %d/%d, want 2/1 (partial denial is not all-denied)", finished.ToolCalls, finished.GateDeniedCalls)
	}
}

// TestToolResultStartingWithDenialPrefixIsNotCountedAsDenial is the
// regression test for the F-C8 false positive: a SUCCESSFUL file_read
// whose content starts with the gate's denial prefix ("Error:
// permission denied for tool ...") must NOT be counted as a gate
// denial. file_read returns file content verbatim, so a file whose
// first line is that prefix — and likewise a fetched page — used to
// be miscounted by the old prefix-sniffing classifier, and five
// such turns disabled the job. The denial is now a typed signal
// from the gate branch of executeTool, so no tool-output content
// can ever count as a denial. The content itself is still fed to
// the model verbatim (asserted on the second provider call's
// messages), and a real denial still counts (see
// TestUnattendedTurnIgnoresAllowRuleAndCountsDenial).
func TestToolResultStartingWithDenialPrefixIsNotCountedAsDenial(t *testing.T) {
	const denialLike = `Error: permission denied for tool "shell_exec": denied: unattended scheduled turn`
	rules := permissions.NewRuleStore("")
	gate := permissions.NewGate(rules, nil)
	readStub := &gateStubTool{name: "file_read", result: denialLike}
	reg := tools.NewRegistry()
	if err := reg.Register(readStub); err != nil {
		t.Fatal(err)
	}
	modelSawContent := false
	prov := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			switch call {
			case 1:
				return toolCallResponse("call-1", "file_read", `{"path":"/tmp/x"}`), nil
			default:
				for _, m := range req.Messages {
					if m.Role == "tool" && m.Content == denialLike {
						modelSawContent = true
					}
				}
				return textResponse("done"), nil
			}
		},
	}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	a.SetPermissionGate(gate)
	ctx := permissions.WithUnattendedContext(permissions.WithApprovalContext(context.Background(), 42, 42))
	var finished ProgressEvent
	out, err := a.RunWithEventHandler(ctx, "read the file", func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			finished = ev
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Fatalf("Run = %q, want %q", out, "done")
	}
	if got := readStub.callCount(); got != 1 {
		t.Errorf("file_read executions = %d, want 1 (the safe tool ran: this was a success, not a denial)", got)
	}
	if !modelSawContent {
		t.Error("the model never saw the file content verbatim: the denial-like result must still reach the model unchanged")
	}
	if finished.ToolCalls != 1 || finished.GateDeniedCalls != 0 {
		t.Errorf("TurnFinished ToolCalls/GateDeniedCalls = %d/%d, want 1/0 (a success whose content starts with the denial prefix is not a denial)", finished.ToolCalls, finished.GateDeniedCalls)
	}
}
