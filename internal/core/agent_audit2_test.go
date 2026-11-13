package core

import (
	"context"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// --- Audit 2: deferred tool calls ----------------------------------------

// TestDeferredToolCallsSurviveLaterMultiCallTurn: calls deferred on an
// earlier turn must not be silently dropped when a later turn also
// requests several calls (the old code overwrote the pending list).
func TestDeferredToolCallsSurviveLaterMultiCallTurn(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(failingTool{}); err != nil {
		t.Fatal(err)
	}
	var reminderReq model.ChatRequest
	prov := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			switch call {
			case 1:
				// Execute echo "first"; defer boom.
				return &model.ChatResponse{
					Message: model.Message{
						Role: "assistant",
						ToolCalls: []model.ToolCall{
							{ID: "call-1", Name: "echo", Arguments: `{"text":"first"}`},
							{ID: "call-2", Name: "boom", Arguments: `{}`},
						},
					},
					FinishReason: "tool_calls",
				}, nil
			case 2:
				// Execute echo "third"; defer echo "fourth".
				// The earlier deferred boom must survive.
				return &model.ChatResponse{
					Message: model.Message{
						Role: "assistant",
						ToolCalls: []model.ToolCall{
							{ID: "call-3", Name: "echo", Arguments: `{"text":"third"}`},
							{ID: "call-4", Name: "echo", Arguments: `{"text":"fourth"}`},
						},
					},
					FinishReason: "tool_calls",
				}, nil
			case 3:
				// Answer directly instead of re-planning: the
				// reminder must list BOTH pending calls.
				return textResponse("early answer"), nil
			case 4:
				reminderReq = req
				// After the reminder, complete the deferred boom.
				return toolCallResponse("call-5", "boom", `{}`), nil
			default:
				return textResponse("done"), nil
			}
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(6), prov, reg, mem)

	out, err := a.Run(context.Background(), "do several things")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Errorf("Run = %q, want %q", out, "done")
	}
	if got := prov.calls(); got != 5 {
		t.Errorf("provider Chat calls = %d, want 5", got)
	}
	if got := echo.callCount(); got != 2 {
		t.Errorf("echo calls = %d, want 2 (first and third; fourth was deferred, never executed)", got)
	}
	if echo.last != "third" {
		t.Errorf("echo last arg = %q, want %q (fourth must never have run)", echo.last, "third")
	}
	// The reminder (in the call-4 request) must name both pending
	// calls: boom (deferred on turn 1) and echo (deferred on turn 2).
	reminded := false
	for _, m := range reminderReq.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "pending") &&
			strings.Contains(m.Content, "boom, echo") {
			reminded = true
		}
	}
	if !reminded {
		t.Errorf("reminder does not list both pending calls (boom, echo): %+v", reminderReq.Messages)
	}
}

// TestDeferredToolCallsSurviveSingleCallTurn: a turn that executes a
// single tool call must not clear the pending list — the model may
// simply not have re-planned the deferred call yet.
func TestDeferredToolCallsSurviveSingleCallTurn(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(failingTool{}); err != nil {
		t.Fatal(err)
	}
	var reminderReq model.ChatRequest
	prov := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			switch call {
			case 1:
				// Execute echo "first"; defer boom.
				return &model.ChatResponse{
					Message: model.Message{
						Role: "assistant",
						ToolCalls: []model.ToolCall{
							{ID: "call-1", Name: "echo", Arguments: `{"text":"first"}`},
							{ID: "call-2", Name: "boom", Arguments: `{}`},
						},
					},
					FinishReason: "tool_calls",
				}, nil
			case 2:
				// A single, unrelated call: boom must stay pending.
				return toolCallResponse("call-3", "echo", `{"text":"other"}`), nil
			case 3:
				// Answer directly: the reminder must still fire
				// for the deferred boom.
				return textResponse("early answer"), nil
			case 4:
				reminderReq = req
				return toolCallResponse("call-4", "boom", `{}`), nil
			default:
				return textResponse("done"), nil
			}
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(6), prov, reg, mem)

	out, err := a.Run(context.Background(), "do several things")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Errorf("Run = %q, want %q", out, "done")
	}
	if got := echo.callCount(); got != 2 {
		t.Errorf("echo calls = %d, want 2", got)
	}
	reminded := false
	for _, m := range reminderReq.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "pending") &&
			strings.Contains(m.Content, "boom") {
			reminded = true
		}
	}
	if !reminded {
		t.Errorf("no reminder for the deferred boom after a single-call turn: %+v", reminderReq.Messages)
	}
}

// --- Audit 2: dangling history repair ------------------------------------

// TestRunRepairsDanglingToolCallHistory: a run that ended after storing
// an assistant tool-call message but before its tool result (cancel,
// crash, failed persist) leaves a dangling call in memory. The next
// run must not send it to the provider: OpenAI and Anthropic both
// reject an assistant message with tool_calls that no tool message
// follows, on every run until the entry ages out of the window.
func TestRunRepairsDanglingToolCallHistory(t *testing.T) {
	mem := newFakeStore()
	if err := mem.AddMessage(model.Message{Role: "user", Content: "do the thing"}); err != nil {
		t.Fatal(err)
	}
	if err := mem.AddMessage(model.Message{
		Role:      "assistant",
		Content:   "",
		ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{"text":"x"}`}},
	}); err != nil {
		t.Fatal(err)
	}
	var sent model.ChatRequest
	prov := &fakeProvider{
		chatFn: func(_ int, req model.ChatRequest) (*model.ChatResponse, error) {
			sent = req
			return textResponse("ok"), nil
		},
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	out, err := a.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "ok" {
		t.Errorf("Run = %q, want %q", out, "ok")
	}
	for _, m := range sent.Messages {
		if len(m.ToolCalls) > 0 {
			t.Errorf("provider received a dangling tool call in history: %+v", m)
		}
		if m.Role == "tool" {
			t.Errorf("provider received a tool message with no preceding call: %+v", m)
		}
	}
}

// TestRunRepairsOrphanToolResult: a tool result whose assistant message
// is not in the window (its turn aged out, or the assistant message
// was never stored) must be dropped, not sent on its own.
func TestRunRepairsOrphanToolResult(t *testing.T) {
	mem := newFakeStore()
	if err := mem.AddMessage(model.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := mem.AddMessage(model.Message{Role: "assistant", Content: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := mem.AddMessage(model.Message{Role: "tool", ToolCallID: "call-9", Content: "orphan result"}); err != nil {
		t.Fatal(err)
	}
	var sent model.ChatRequest
	prov := &fakeProvider{
		chatFn: func(_ int, req model.ChatRequest) (*model.ChatResponse, error) {
			sent = req
			return textResponse("ok"), nil
		},
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	if _, err := a.Run(context.Background(), "next"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range sent.Messages {
		if m.Role == "tool" {
			t.Errorf("provider received an orphan tool result: %+v", m)
		}
	}
	// The surrounding well-formed history must survive the repair.
	found := false
	for _, m := range sent.Messages {
		if m.Role == "assistant" && m.Content == "ok" {
			found = true
		}
	}
	if !found {
		t.Errorf("repair dropped the well-formed assistant message: %+v", sent.Messages)
	}
}

// TestRunKeepsWellFormedToolHistory: a complete assistant tool-call /
// tool-result pair in history must pass through the repair untouched.
func TestRunKeepsWellFormedToolHistory(t *testing.T) {
	mem := newFakeStore()
	if err := mem.AddMessage(model.Message{Role: "user", Content: "do it"}); err != nil {
		t.Fatal(err)
	}
	if err := mem.AddMessage(model.Message{
		Role:      "assistant",
		ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{"text":"x"}`}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mem.AddMessage(model.Message{Role: "tool", ToolCallID: "call-1", Content: "echo:x"}); err != nil {
		t.Fatal(err)
	}
	var sent model.ChatRequest
	prov := &fakeProvider{
		chatFn: func(_ int, req model.ChatRequest) (*model.ChatResponse, error) {
			sent = req
			return textResponse("ok"), nil
		},
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	if _, err := a.Run(context.Background(), "and then?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var assistantWithCall, toolResult bool
	for _, m := range sent.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "call-1" {
			assistantWithCall = true
		}
		if m.Role == "tool" && m.ToolCallID == "call-1" && m.Content == "echo:x" {
			toolResult = true
		}
	}
	if !assistantWithCall {
		t.Errorf("repair stripped the well-formed assistant tool call: %+v", sent.Messages)
	}
	if !toolResult {
		t.Errorf("repair dropped the well-formed tool result: %+v", sent.Messages)
	}
}

// TestRepairHistoryIsPositional: satisfaction is positional, not a
// global ID set — Ollama synthesises call-1, call-2, ... per turn, so
// the same ID can appear in several turns. A result must only satisfy
// the assistant message it immediately follows.
func TestRepairHistoryIsPositional(t *testing.T) {
	msgs := []model.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "call-1", Content: "echo:x"},
		{Role: "user", Content: "second"},
		// Same ID, later turn, no result: dangling, must be dropped.
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{}`}}},
	}
	out, repaired := repairHistory(msgs)
	if repaired != 1 {
		t.Errorf("repairHistory repaired %d, want 1 (only the second call-1 is dangling)", repaired)
	}
	if len(out) != 4 {
		t.Fatalf("repairHistory returned %d messages, want 4: %+v", len(out), out)
	}
	if out[1].Role != "assistant" || len(out[1].ToolCalls) != 1 {
		t.Errorf("repairHistory dropped the satisfied first call: %+v", out)
	}
	if out[2].Role != "tool" || out[2].ToolCallID != "call-1" {
		t.Errorf("repairHistory dropped the first tool result: %+v", out)
	}
}

// TestRepairHistoryKeepsTextOfStrippedCall: an assistant message with
// content whose tool calls are all dangling is kept as plain text.
func TestRepairHistoryKeepsTextOfStrippedCall(t *testing.T) {
	msgs := []model.Message{
		{Role: "assistant", Content: "I'll check that.", ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{}`}}},
	}
	out, repaired := repairHistory(msgs)
	if repaired != 1 {
		t.Errorf("repairHistory repaired %d, want 1", repaired)
	}
	if len(out) != 1 || out[0].Content != "I'll check that." || len(out[0].ToolCalls) != 0 {
		t.Errorf("repairHistory = %+v, want the text kept with no tool calls", out)
	}
}
