package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/subagent"
	"github.com/irfan/orion/internal/tools"
)

// scriptedProvider is a minimal model.Provider for subagent tests:
// Chat is driven by chatFn; Stream is unused by these tests and
// reports an error if called.
type scriptedProvider struct {
	name   string
	chatFn func(call int, req model.ChatRequest) (*model.ChatResponse, error)
	calls  int
}

func (p *scriptedProvider) Name() string { return p.name }
func (p *scriptedProvider) Chat(_ context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.calls++
	return p.chatFn(p.calls, req)
}
func (p *scriptedProvider) Stream(_ context.Context, _ model.ChatRequest, _ func(chunk string) error) (*model.ChatResponse, error) {
	return nil, errors.New("scriptedProvider: Stream not implemented for this test")
}

func testSubagentConfig(maxSteps int) *config.Config {
	cfg := config.Default()
	cfg.MaxSteps = maxSteps
	cfg.TimeoutSeconds = 30
	return cfg
}

func toolNamesInRequest(req model.ChatRequest) map[string]bool {
	names := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		names[t.Name] = true
	}
	return names
}

// TestSubagentExecutorRealChildLoop: the executor runs a REAL child
// agent loop against a fake provider — no live calls — and returns
// its real answer with the usage the child's TurnFinished reported.
func TestSubagentExecutorRealChildLoop(t *testing.T) {
	childProv := &scriptedProvider{
		name: "child",
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			names := toolNamesInRequest(req)
			for _, n := range []string{"subagent_spawn", "subagent_list", "subagent_result", "subagent_stop"} {
				if names[n] {
					t.Errorf("child request exposes subagent tool %q (recursion must be impossible by construction)", n)
				}
			}
			if call == 1 {
				// The child model tries to delegate further: the
				// tool does not exist, so the loop feeds the
				// unknown-tool error back and the child must still
				// answer on the next step.
				return &model.ChatResponse{
					Message: model.Message{
						Role:      "assistant",
						ToolCalls: []model.ToolCall{{ID: "c1", Name: "subagent_spawn", Arguments: `{"brief":"nested work"}`}},
					},
					FinishReason: "tool_calls",
				}, nil
			}
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: "child-answer"},
				FinishReason: "stop",
				Usage:        model.Usage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42},
			}, nil
		},
	}
	exec := newSubagentExecutor(childProv, testSubagentConfig(5), nil, nil)
	out, usage, err := exec(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	if out != "child-answer" {
		t.Fatalf("output = %q, want %q", out, "child-answer")
	}
	if usage.Tokens != 42 {
		t.Fatalf("usage.Tokens = %d, want 42 (child TurnFinished total)", usage.Tokens)
	}
	if childProv.calls != 2 {
		t.Fatalf("child provider calls = %d, want 2 (recursion attempt + final answer)", childProv.calls)
	}
}

// TestSubagentExecutorPropagatesMaxStepsError: when the child exhausts
// its step budget, the executor returns the MaxStepsError as the
// error — never as output.
func TestSubagentExecutorPropagatesMaxStepsError(t *testing.T) {
	childProv := &scriptedProvider{
		name: "child",
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			// Always request one more (valid) tool call, so the
			// loop never produces a final answer.
			return &model.ChatResponse{
				Message: model.Message{
					Role:      "assistant",
					ToolCalls: []model.ToolCall{{ID: "c1", Name: "calculator", Arguments: `{"expression":"1+1"}`}},
				},
				FinishReason: "tool_calls",
			}, nil
		},
	}
	exec := newSubagentExecutor(childProv, testSubagentConfig(2), nil, nil)
	out, _, err := exec(context.Background(), "loop forever")
	if out != "" {
		t.Fatalf("output = %q, want empty when the child errors", out)
	}
	if err == nil {
		t.Fatal("executor error = nil, want MaxStepsError")
	}
	var maxErr *core.MaxStepsError
	if !errors.As(err, &maxErr) {
		t.Fatalf("executor error = %T (%v), want *core.MaxStepsError", err, err)
	}
}

// TestSubagentExecutorChildRegistryHasNoSubagentTools: the registry
// the factory builds for children exposes none of the four subagent
// tools, so a child cannot spawn subagents by construction.
func TestSubagentExecutorChildRegistryHasNoSubagentTools(t *testing.T) {
	reg, err := newChildRegistry(testSubagentConfig(5))
	if err != nil {
		t.Fatalf("newChildRegistry: %v", err)
	}
	for _, n := range []string{"subagent_spawn", "subagent_list", "subagent_result", "subagent_stop"} {
		if _, ok := reg.Get(n); ok {
			t.Errorf("child registry contains %q", n)
		}
	}
	if _, ok := reg.Get("calculator"); !ok {
		t.Error("child registry lost ordinary tools (calculator missing)")
	}
}

// TestReadOnlyTypedSource: the view handed to children delegates List
// and Load to the parent's store, but does NOT expose
// MarkTypedMemoryStale — core's drift check only calls that method
// when the source offers it, so a child can never persist a stale
// marker into the parent's typed store.
func TestReadOnlyTypedSource(t *testing.T) {
	dir := t.TempDir()
	ts, err := memory.NewTypedStore(dir)
	if err != nil {
		t.Fatalf("NewTypedStore: %v", err)
	}
	if err := ts.Save(memory.TypedMemory{
		Name: "rule-one", Description: "a test rule", Type: memory.TypeFeedback, Body: "answer in one sentence\nWhy: keeps replies short\nHow to apply: keep every reply to one sentence",
	}); err != nil {
		t.Fatalf("TypedStore.Save: %v", err)
	}
	view := &readOnlyTypedSource{src: newTypedStoreAdapter(ts)}
	if _, ok := any(view).(interface{ MarkTypedMemoryStale(string) error }); ok {
		t.Fatal("readOnlyTypedSource exposes MarkTypedMemoryStale — a child could write to shared typed memory")
	}
	list, err := view.ListTypedMemories()
	if err != nil || len(list) != 1 || list[0].Name != "rule-one" {
		t.Fatalf("ListTypedMemories = %+v, %v; want the one saved memory", list, err)
	}
	loaded, err := view.LoadTypedMemory("rule-one")
	if err != nil || !strings.Contains(loaded.Body, "answer in one sentence") {
		t.Fatalf("LoadTypedMemory = %+v, %v; want the saved body", loaded, err)
	}
}

// TestSubagentManagerEndToEndUsageAndIsolation wires the whole chain
// the way Build does — parent agent + Worker A's tools + the child
// factory — with scripted providers (no live calls):
//   - the parent delegates a computation, polls, and answers;
//   - the child's TurnFinished tokens land on the parent's TurnStats
//     via the sink (parent provider tokens + child tokens);
//   - the parent's durable history contains no child turn: the only
//     user-role messages are the seeded one and the Run input —
//     the child's own user message (its brief, as a turn) never
//     appears there.
func TestSubagentManagerEndToEndUsageAndIsolation(t *testing.T) {
	dir := t.TempDir()
	parentMem, err := memory.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := parentMem.AddMessage(model.Message{Role: "user", Content: "seeded parent context"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := parentMem.AddMessage(model.Message{Role: "assistant", Content: "noted"}); err != nil {
		t.Fatalf("seed assistant: %v", err)
	}
	cfg := testSubagentConfig(10)

	childProv := &scriptedProvider{
		name: "child",
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: "child-result-6x7-42"},
				FinishReason: "stop",
				Usage:        model.Usage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42},
			}, nil
		},
	}
	mgr := subagent.NewManagerWithExecutor(4, newSubagentExecutor(childProv, cfg, nil, nil))
	defer mgr.Shutdown()

	parentProv := &scriptedProvider{
		name: "parent",
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			usage := model.Usage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10}
			switch {
			case call == 1:
				return &model.ChatResponse{
					Message: model.Message{
						Role:      "assistant",
						ToolCalls: []model.ToolCall{{ID: "p1", Name: "subagent_spawn", Arguments: `{"brief":"compute 6*7","timeout_seconds":60}`}},
					},
					FinishReason: "tool_calls",
					Usage:        usage,
				}, nil
			default:
				// Find the latest tool result: spawn returns the
				// task id; result returns running (poll again) or
				// done (answer). The scripted model re-issues
				// subagent_result until the task is done.
				msgs := req.Messages
				lastTool := ""
				for i := len(msgs) - 1; i >= 0; i-- {
					if msgs[i].Role == "tool" {
						lastTool = msgs[i].Content
						break
					}
				}
				var parsed map[string]any
				_ = json.Unmarshal([]byte(lastTool), &parsed)
				if id, _ := parsed["id"].(string); id != "" {
					if status, _ := parsed["status"].(string); status == "done" {
						return &model.ChatResponse{
							Message:      model.Message{Role: "assistant", Content: "parent-done"},
							FinishReason: "stop",
							Usage:        usage,
						}, nil
					}
					return &model.ChatResponse{
						Message: model.Message{
							Role:      "assistant",
							ToolCalls: []model.ToolCall{{ID: "p2", Name: "subagent_result", Arguments: `{"id":"` + id + `"}`}},
						},
						FinishReason: "tool_calls",
						Usage:        usage,
					}, nil
				}
				t.Fatalf("call %d: no task id in last tool result %q", call, lastTool)
				return nil, errors.New("unreachable")
			}
		},
	}
	parentReg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	parentAgent, err := core.NewAgent(cfg, parentProv, parentReg, parentMem)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	subTools, err := tools.NewSubagentTools(mgr, parentAgent)
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	for _, tl := range subTools {
		if err := parentReg.Register(tl); err != nil {
			t.Fatalf("Register %s: %v", tl.Name(), err)
		}
	}
	var finished core.ProgressEvent
	var finishedSeen bool
	parentAgent.SetEventHandler(func(ev core.ProgressEvent) {
		if ev.Kind == core.EventTurnFinished {
			finished, finishedSeen = ev, true
		}
	})
	reply, err := parentAgent.Run(context.Background(), "delegate the computation")
	if err != nil {
		t.Fatalf("parent Run: %v", err)
	}
	if reply != "parent-done" {
		t.Fatalf("reply = %q, want %q", reply, "parent-done")
	}
	if !finishedSeen {
		t.Fatal("no TurnFinished event on the parent")
	}
	// Parent provider reported 10 tokens per call; the child
	// reported 42 once, charged when the parent consumed the
	// result. The TurnFinished total is the sum.
	wantTotal := parentProv.calls*10 + 42
	if finished.Usage.TotalTokens != wantTotal {
		t.Fatalf("TurnFinished TotalTokens = %d, want %d (%d parent calls x 10 + 42 child)",
			finished.Usage.TotalTokens, wantTotal, parentProv.calls)
	}

	// History isolation: the parent's durable history must hold no
	// child turn. The child's brief appears only inside the
	// parent's own tool-call arguments (the parent's delegation),
	// never as a user-role message — the child ran its brief as its
	// own turn in its own ephemeral store.
	stored, err := parentMem.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	userMsgs := 0
	for _, m := range stored {
		if m.Role == "user" {
			userMsgs++
			if m.Content == "compute 6*7" {
				t.Fatalf("parent history contains the child's brief as a user turn — child history leaked: %+v", m)
			}
		}
	}
	if userMsgs != 2 {
		t.Fatalf("user-role messages in parent history = %d, want 2 (seeded + Run input)", userMsgs)
	}
	if len(stored) < 2 {
		t.Fatalf("parent history has %d messages, want at least the 2 seeded ones", len(stored))
	}

	// /agents rendering (same data Telegram /agents and `orion
	// agents` show): the finished task appears with a short id,
	// status, tokens and a result summary.
	tasks := mgr.List()
	if len(tasks) != 1 {
		t.Fatalf("manager tasks = %d, want 1", len(tasks))
	}
	a := &App{Subagents: mgr}
	summary := a.AgentsSummary()
	if !strings.Contains(summary, tasks[0].ID[:8]) {
		t.Fatalf("AgentsSummary missing short id %q:\n%s", tasks[0].ID[:8], summary)
	}
	if !strings.Contains(summary, "[done]") {
		t.Fatalf("AgentsSummary missing [done] status:\n%s", summary)
	}
	if !strings.Contains(summary, "42 tokens") {
		t.Fatalf("AgentsSummary missing child tokens:\n%s", summary)
	}
	if !strings.Contains(summary, "child-result-6x7-42") {
		t.Fatalf("AgentsSummary missing result summary:\n%s", summary)
	}
}
