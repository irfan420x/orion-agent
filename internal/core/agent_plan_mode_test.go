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

// planBlockerFake is a minimal Tool implementing PlanModeChecker for
// the agent-loop tests below: it blocks exactly one tool name, so
// the tests pin the executeTool seam without needing the real
// plan-mode tool (its Telegram machinery lives in the tools
// package and is tested there).
type planBlockerFake struct{}

func (planBlockerFake) Name() string { return "plancheck_fake" }

func (planBlockerFake) Description() string {
	return "test-only tool that blocks shell_exec while a plan is pending"
}

func (planBlockerFake) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "plancheck_fake",
		Description: "test-only tool that blocks shell_exec while a plan is pending",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (planBlockerFake) RiskClass() permissions.Class { return permissions.ClassSafe }

func (planBlockerFake) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}

func (planBlockerFake) PlanModeBlocks(ctx context.Context, tool string) (bool, string) {
	if tool == "shell_exec" {
		return true, "a test plan is pending approval for this chat"
	}
	return false, ""
}

func TestAgentPlanModeBlocksDestructiveTool(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(planBlockerFake{}); err != nil {
		t.Fatalf("Register(planBlockerFake): %v", err)
	}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, reg, newFakeStore())

	got := a.executeTool(context.Background(), model.ToolCall{
		ID:        "1",
		Name:      "shell_exec",
		Arguments: `{"command":"echo hi"}`,
	})
	if !strings.Contains(got, "blocked while a plan is pending approval") {
		t.Errorf("executeTool(shell_exec) with a pending plan = %q, want the plan-mode block text", got)
	}

	// The checker itself and other tools are not blocked.
	got = a.executeTool(context.Background(), model.ToolCall{
		ID:        "2",
		Name:      "plancheck_fake",
		Arguments: `{}`,
	})
	if got != "ok" {
		t.Errorf("executeTool(plancheck_fake) = %q, want %q", got, "ok")
	}
}

func TestAgentPlanModeNoCheckerPassesThrough(t *testing.T) {
	// A registry without any PlanModeChecker never blocks: build
	// one with no tools at all and confirm the block text never
	// appears (the call fails as unknown, not as plan-blocked).
	empty := tools.NewRegistry()
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, empty, newFakeStore())

	got := a.executeTool(context.Background(), model.ToolCall{
		ID:        "1",
		Name:      "shell_exec",
		Arguments: `{"command":"echo hi"}`,
	})
	if strings.Contains(got, "blocked while a plan is pending approval") {
		t.Errorf("executeTool(shell_exec) with no checker = %q, want no plan-mode block", got)
	}
}

// --- real plan tool, end to end ------------------------------------------

// planModeFakeGateway records plan messages the real plan tool
// sends, so the test can drive the decision from the outside.
type planModeFakeGateway struct {
	mu      sync.Mutex
	sends   []tools.InlineKeyboard
	sendCh  chan struct{}
	nextMsg int64
}

func (f *planModeFakeGateway) SendText(ctx context.Context, chatID int64, text string) (int64, error) {
	return 0, nil
}

func (f *planModeFakeGateway) SendKeyboard(ctx context.Context, chatID int64, text string, keyboard tools.InlineKeyboard) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextMsg++
	f.sends = append(f.sends, keyboard)
	select {
	case f.sendCh <- struct{}{}:
	default:
	}
	return f.nextMsg, nil
}

func (f *planModeFakeGateway) EditTextRemoveKeyboard(ctx context.Context, chatID, messageID int64, htmlText string) error {
	return nil
}

func (f *planModeFakeGateway) AnswerCallback(ctx context.Context, callbackID, text string) error {
	return nil
}

type planModeFakeCallbackQuery struct {
	data   string
	id     string
	sender int64
	chat   int64
	msg    int64
}

func (q planModeFakeCallbackQuery) CallbackData() string { return q.data }
func (q planModeFakeCallbackQuery) CallbackID() string   { return q.id }
func (q planModeFakeCallbackQuery) SenderID() int64      { return q.sender }
func (q planModeFakeCallbackQuery) HasMessage() bool     { return q.msg != 0 }
func (q planModeFakeCallbackQuery) MessageChatID() int64 { return q.chat }
func (q planModeFakeCallbackQuery) MessageID() int64     { return q.msg }

func TestAgentPlanModeRealToolBlocksExecution(t *testing.T) {
	store := tools.NewPlanningStore("")
	reg, err := tools.DefaultRegistryWithDeps(tools.ToolDeps{PlanningStore: store})
	if err != nil {
		t.Fatalf("DefaultRegistryWithDeps: %v", err)
	}
	enterTool, ok := reg.Get("enter_plan_mode")
	if !ok {
		t.Fatal("registry has no enter_plan_mode")
	}
	gw := &planModeFakeGateway{sendCh: make(chan struct{}, 1)}
	ta, ok := enterTool.(tools.TransportAware)
	if !ok {
		t.Fatal("enter_plan_mode does not implement TransportAware")
	}
	if err := ta.InstallTransport(&tools.ToolHost{Gateway: gw, OwnerID: 999}); err != nil {
		t.Fatalf("InstallTransport: %v", err)
	}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, reg, newFakeStore())
	ctx := permissions.WithApprovalContext(context.Background(), 100, 100)

	// Enter plan mode in the background: it blocks waiting for
	// the user's decision.
	type enterOutcome struct {
		res string
		err error
	}
	enterCh := make(chan enterOutcome, 1)
	go func() {
		res, err := reg.Execute(ctx, "enter_plan_mode", json.RawMessage(`{"goal":"Do a thing","steps":["Step one"]}`))
		enterCh <- enterOutcome{res: res, err: err}
	}()
	select {
	case <-gw.sendCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the plan message")
	}

	// While the plan is pending, a destructive call through the
	// agent loop is refused — and a safe call is not.
	got := a.executeTool(ctx, model.ToolCall{ID: "1", Name: "shell_exec", Arguments: `{"command":"echo hi"}`})
	if !strings.Contains(got, "blocked while a plan is pending approval") {
		t.Errorf("executeTool(shell_exec) with a pending plan = %q, want the plan-mode block text", got)
	}
	got = a.executeTool(ctx, model.ToolCall{ID: "2", Name: "file_read", Arguments: `{"path":"/nonexistent-xyz"}`})
	if strings.Contains(got, "blocked while a plan is pending approval") {
		t.Errorf("executeTool(file_read) with a pending plan = %q, want no plan-mode block (safe tools stay allowed)", got)
	}

	// The user approves: the plan resolves, and the block lifts.
	gw.mu.Lock()
	if len(gw.sends) == 0 {
		gw.mu.Unlock()
		t.Fatal("no plan message recorded")
	}
	kb := gw.sends[0]
	gw.mu.Unlock()
	var planID string
	for _, row := range kb.Rows {
		for _, b := range row {
			if strings.HasPrefix(b.CallbackData, "plan:approve:") {
				planID = strings.TrimPrefix(b.CallbackData, "plan:approve:")
			}
		}
	}
	if planID == "" {
		t.Fatal("no approve button in the recorded plan keyboard")
	}
	handler, ok := enterTool.(tools.CallbackHandler)
	if !ok {
		t.Fatal("enter_plan_mode does not implement CallbackHandler")
	}
	if err := handler.HandleCallback(ctx, gw, planModeFakeCallbackQuery{
		data: "plan:approve:" + planID, id: "cb1", sender: 100, chat: 100, msg: 1,
	}); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	select {
	case out := <-enterCh:
		if out.err != nil {
			t.Fatalf("enter_plan_mode: %v", out.err)
		}
		if !strings.Contains(out.res, `"decision":"approved"`) {
			t.Errorf("enter_plan_mode result = %q, want an approval", out.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for enter_plan_mode to return after approval")
	}
	if blocked, _ := reg.PlanModeBlocks(ctx, "shell_exec"); blocked {
		t.Error("PlanModeBlocks(shell_exec) after approval = true, want false")
	}
}
