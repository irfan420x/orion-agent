package core

import (
	"context"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// --- Hardening tests (audit P0-5, central guard) ---

// TestRunSynthesisesEmptyToolCallIDs: a provider that returns a tool
// call with an empty ID must not break tool-result linkage — the agent
// loop synthesises a stable ID centrally, so history converts cleanly
// for OpenAI/Anthropic on the next turn.
func TestRunSynthesisesEmptyToolCallIDs(t *testing.T) {
	provider := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("", "calculator", `{"expression":"2+2"}`), nil
			}
			return textResponse("four"), nil
		},
	}
	mem := newFakeStore()
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	agent := newTestAgent(t, nil, provider, reg, mem)
	reply, err := agent.Run(context.Background(), "what is 2+2?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "four" {
		t.Fatalf("reply = %q, want four", reply)
	}
	recent, err := mem.RecentMessages(10)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	var toolMsg *model.Message
	for i := range recent {
		if recent[i].Role == "tool" {
			toolMsg = &recent[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message recorded in memory")
	}
	if toolMsg.ToolCallID == "" {
		t.Fatal("tool message has an empty ToolCallID (provider omitted the ID and the loop did not synthesise one)")
	}
	if !strings.HasPrefix(toolMsg.ToolCallID, "call-") {
		t.Fatalf("ToolCallID = %q, want a synthesised call-* id", toolMsg.ToolCallID)
	}
	if !strings.Contains(toolMsg.Content, "4") {
		t.Fatalf("tool result = %q, want it to contain the calculator result", toolMsg.Content)
	}
}
