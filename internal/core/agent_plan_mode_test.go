package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
