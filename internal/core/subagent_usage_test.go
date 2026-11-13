package core

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// chargeTool is a test tool that reports subagent tokens to the agent
// mid-turn, the way the subagent_result tool does when the parent
// consumes a finished subagent result.
type chargeTool struct {
	agent  *Agent
	tokens int
}

func (t *chargeTool) Name() string        { return "charge" }
func (t *chargeTool) Description() string { return "Reports subagent tokens to the agent." }
func (t *chargeTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "charge", Description: t.Description(), Parameters: map[string]any{"type": "object"}}
}
func (t *chargeTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	t.agent.AddSubagentUsage(t.tokens)
	return "charged", nil
}

// TestSubagentUsageAddedToTurnFinished: tokens reported to the agent
// during a turn (via AddSubagentUsage) land on the turn's
// TurnFinished Usage.TotalTokens — and only there: PromptTokens and
// CompletionTokens are untouched, because a child reports one total.
func TestSubagentUsageAddedToTurnFinished(t *testing.T) {
	prov := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("c1", "charge", "{}"), nil
			}
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: "done"},
				FinishReason: "stop",
				Usage:        model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			}, nil
		},
	}
	reg := tools.NewRegistry()
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	if err := reg.Register(&chargeTool{agent: a, tokens: 42}); err != nil {
		t.Fatalf("Register charge tool: %v", err)
	}
	var finished ProgressEvent
	var finishedSeen bool
	a.SetEventHandler(func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			finished, finishedSeen = ev, true
		}
	})
	if _, err := a.Run(context.Background(), "do it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !finishedSeen {
		t.Fatal("no TurnFinished event")
	}
	// Parent provider reported 15 total on the final step (the tool
	// step reported zero usage); the 42 subagent tokens are added.
	if finished.Usage.TotalTokens != 15+42 {
		t.Fatalf("TurnFinished TotalTokens = %d, want %d (15 parent + 42 subagent)", finished.Usage.TotalTokens, 15+42)
	}
	if finished.Usage.PromptTokens != 10 || finished.Usage.CompletionTokens != 5 {
		t.Fatalf("TurnFinished Prompt/Completion = %d/%d, want 10/5 (subagent tokens go to Total only)",
			finished.Usage.PromptTokens, finished.Usage.CompletionTokens)
	}
}

// TestSubagentUsageResetsPerTurn: tokens added after a turn finished
// (a late subagent completion) are dropped by the next turn's reset,
// never carried into the next turn's accounting.
func TestSubagentUsageResetsPerTurn(t *testing.T) {
	prov := &fakeProvider{}
	reg := tools.NewRegistry()
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	var totals []int
	a.SetEventHandler(func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			totals = append(totals, ev.Usage.TotalTokens)
		}
	})
	if _, err := a.Run(context.Background(), "first"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Late completion: reported after the turn ended, before the next.
	a.AddSubagentUsage(99)
	if _, err := a.Run(context.Background(), "second"); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("TurnFinished events = %d, want 2", len(totals))
	}
	// fakeProvider reports zero usage, so both turns must be zero:
	// the late 99 must not leak into the second turn.
	if totals[0] != 0 || totals[1] != 0 {
		t.Fatalf("turn totals = %v, want [0 0]", totals)
	}
}

// TestAddSubagentUsageRaceSafe: concurrent reporters (subagents
// finishing at the same time) must not lose tokens or race. The
// reported total is observed by a turn whose tool call fans the
// reports out — here, many goroutines report directly and a
// following turn's TurnFinished must show the full sum.
func TestAddSubagentUsageRaceSafe(t *testing.T) {
	prov := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("c1", "racecharge", "{}"), nil
			}
			return textResponse("done"), nil
		},
	}
	reg := tools.NewRegistry()
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	// The charge tool itself fans out 50 concurrent reporters.
	racer := &raceChargeTool{agent: a, n: 50, tokens: 3}
	if err := reg.Register(racer); err != nil {
		t.Fatalf("Register race tool: %v", err)
	}
	var total int
	a.SetEventHandler(func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			total = ev.Usage.TotalTokens
		}
	})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if total != 50*3 {
		t.Fatalf("TurnFinished TotalTokens = %d, want %d (50 concurrent reporters x 3)", total, 50*3)
	}
}

// raceChargeTool reports tokens from many goroutines at once.
type raceChargeTool struct {
	agent  *Agent
	n      int
	tokens int
}

func (t *raceChargeTool) Name() string        { return "racecharge" }
func (t *raceChargeTool) Description() string { return "Reports subagent tokens concurrently." }
func (t *raceChargeTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "racecharge", Description: t.Description(), Parameters: map[string]any{"type": "object"}}
}
func (t *raceChargeTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	var wg sync.WaitGroup
	for i := 0; i < t.n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.agent.AddSubagentUsage(t.tokens)
		}()
	}
	wg.Wait()
	return "charged", nil
}
