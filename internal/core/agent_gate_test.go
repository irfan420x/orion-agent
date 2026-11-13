package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// --- Permission gate tests (P1-O2) --------------------------------------

// gateStubTool is a named tool that records executions. Its name
// decides its gate class (shell_exec/file_write are gated, echo is
// not), so one stub covers the gated and ungated paths.
type gateStubTool struct {
	mu    sync.Mutex
	name  string
	calls int
}

func (t *gateStubTool) Name() string        { return t.name }
func (t *gateStubTool) Description() string { return "test tool " + t.name }
func (t *gateStubTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: t.name, Description: t.Description()}
}
func (t *gateStubTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	return "ok:" + t.name, nil
}
func (t *gateStubTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func TestGateDeniesToolCallAndLoopContinues(t *testing.T) {
	rules := permissions.NewRuleStore("")
	if err := rules.Set("shell_exec", permissions.ActionDeny); err != nil {
		t.Fatal(err)
	}
	gate := permissions.NewGate(rules, nil)
	stub := &gateStubTool{name: "shell_exec"}
	reg := tools.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "shell_exec", `{"command":"echo hi"}`), nil
			}
			return textResponse("done"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)
	a.SetPermissionGate(gate)
	ctx := permissions.WithApprovalContext(context.Background(), 42, 42)
	out, err := a.Run(ctx, "run the shell")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Fatalf("Run = %q, want %q (the loop must continue after a denial, not crash)", out, "done")
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("shell_exec executions = %d, want 0 (denied before Execute)", got)
	}
	// The denial must reach the model as a tool result, worded as a
	// denial, so it can explain instead of retrying blindly.
	found := false
	for _, m := range mem.allMessages() {
		if m.Role == "tool" && strings.Contains(m.Content, "permission denied") {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'permission denied' tool result in memory: %+v", mem.allMessages())
	}
}

func TestGateApprovesToolCall(t *testing.T) {
	var gate *permissions.Gate
	gate = permissions.NewGate(permissions.NewRuleStore(""), func(ctx context.Context, req permissions.ApprovalRequest) error {
		go func() {
			time.Sleep(10 * time.Millisecond)
			gate.Resolve(req.ID, true, nil)
		}()
		return nil
	})
	stub := &gateStubTool{name: "shell_exec"}
	reg := tools.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "shell_exec", `{"command":"echo hi"}`), nil
			}
			return textResponse("done"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)
	a.SetPermissionGate(gate)
	ctx := permissions.WithApprovalContext(context.Background(), 42, 42)
	if _, err := a.Run(ctx, "run the shell"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("shell_exec executions = %d, want 1 (approved)", got)
	}
}

func TestGateDenylistAbsoluteUnderApproveAllGate(t *testing.T) {
	// The approver approves everything and there are no rules: the
	// denylist must still refuse the call, without prompting.
	prompted := false
	gate := permissions.NewGate(permissions.NewRuleStore(""), func(ctx context.Context, req permissions.ApprovalRequest) error {
		prompted = true
		return nil
	})
	stub := &gateStubTool{name: "shell_exec"}
	reg := tools.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "shell_exec", `{"command":"rm -rf /"}`), nil
			}
			return textResponse("done"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)
	a.SetPermissionGate(gate)
	ctx := permissions.WithApprovalContext(context.Background(), 42, 42)
	if _, err := a.Run(ctx, "run the shell"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("denylisted shell_exec executions = %d, want 0", got)
	}
	if prompted {
		t.Error("denylisted command prompted the user")
	}
	denied := false
	for _, m := range mem.allMessages() {
		if m.Role == "tool" && strings.Contains(m.Content, "denylist") {
			denied = true
		}
	}
	if !denied {
		t.Errorf("no denylist denial in memory: %+v", mem.allMessages())
	}
}

func TestGateSafeToolRunsWithoutPrompt(t *testing.T) {
	prompted := false
	gate := permissions.NewGate(permissions.NewRuleStore(""), func(ctx context.Context, req permissions.ApprovalRequest) error {
		prompted = true
		return nil
	})
	stub := &gateStubTool{name: "echo"}
	reg := tools.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "echo", `{"text":"hi"}`), nil
			}
			return textResponse("done"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)
	a.SetPermissionGate(gate)
	if _, err := a.Run(context.Background(), "echo hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("echo executions = %d, want 1", got)
	}
	if prompted {
		t.Error("safe tool prompted the user")
	}
}

func TestNoGateRunsUngatedAsBefore(t *testing.T) {
	// No gate installed (the CLI configuration): a shell tool runs
	// exactly as it did before P1-O2.
	stub := &gateStubTool{name: "shell_exec"}
	reg := tools.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "shell_exec", `{"command":"echo hi"}`), nil
			}
			return textResponse("done"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)
	if _, err := a.Run(context.Background(), "run the shell"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("shell_exec executions = %d, want 1 (no gate installed)", got)
	}
}
