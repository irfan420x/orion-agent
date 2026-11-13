// W3b (rc10) regression tests for Audit 5 budget findings F-A5-6
// (projection + mid-turn abort), F-A5-7 (subagent sweep) and
// F-A5-P2-18 (per-model /usage pricing).
package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/subagent"
	"github.com/irfan/orion/internal/tools"
)

// TestW3BProjectionRefusesNearLimitTurn proves the F-A5-6 pre-flight
// projection: with a 100,000-token limit and 99,999 already spent,
// even a short turn is refused before the provider is called, and
// the day's spend does not move at all — the old Check(chatID, 0)
// would have let the turn run and blow past the limit.
func TestW3BProjectionRefusesNearLimitTurn(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("ok", model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}), nil
	}}
	a.Provider = prov
	if err := a.Budget.RecordUsage(111, model.Usage{TotalTokens: 99999}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, err := a.HandleStreamForChat(context.Background(), 111, "please summarise the whole history for me", nil); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("HandleStreamForChat err = %v, want ErrBudgetExceeded", err)
	}
	if got := prov.Calls(); got != 0 {
		t.Errorf("provider calls = %d, want 0 (the refused turn must never reach the provider)", got)
	}
	if got := a.Budget.Usage(111); got != 99999 {
		t.Errorf("budget Usage after refused turn = %d, want 99,999 (spend must not blow past the limit)", got)
	}
}

// TestW3BSubagentSweepChargesUnfetchedTasks proves F-A5-7: a child
// task the parent spawns and never fetches is still charged, at
// turn end, by the per-chat sweep in runChatTurn. The child finishes
// mid-turn (the second provider call waits for it), the parent never
// calls subagent_result, and the turn's stats must still carry the
// child's 250 tokens — exactly once, so a second turn charges 0.
func TestW3BSubagentSweepChargesUnfetchedTasks(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return "child done", subagent.Usage{Tokens: 250}, nil
	})
	a.Subagents = mgr
	subTools, err := tools.NewSubagentTools(mgr, tools.UsageSinkFunc(func(int) {}))
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	for _, tool := range subTools {
		if err := a.Tools.Register(tool); err != nil {
			t.Fatalf("Register %s: %v", tool.Name(), err)
		}
	}
	// The second provider call waits until the spawned child has
	// finished, so the child deterministically completes mid-turn
	// (the test goroutine releases it; it never waits longer than
	// 5s, so a stuck child fails the test instead of hanging it).
	childDone := make(chan struct{})
	var sawDone atomic.Bool
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, ts := range mgr.List() {
				if ts.Status == subagent.StatusDone {
					sawDone.Store(true)
					close(childDone)
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		close(childDone)
	}()
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		switch call {
		case 1:
			return &model.ChatResponse{
				Message: model.Message{
					Role:      "assistant",
					ToolCalls: []model.ToolCall{{ID: "c1", Name: "subagent_spawn", Arguments: `{"brief":"count the words in the notes"}`}},
				},
				FinishReason: "tool_calls",
			}, nil
		case 2:
			<-childDone
			return finalUsageResponse("done", model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}), nil
		default:
			return finalUsageResponse("done again", model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}), nil
		}
	}}
	a.Provider = prov

	// The turn ctx carries the approval context exactly as the
	// daemon sets it (main.go WithApprovalContext): without it
	// the spawned task is unattributed (chatID 0) and the sweep
	// correctly charges it to nobody — so the wrapper is load
	// bearing, not decoration.
	_, stats, err := a.HandleStreamForChat(permissions.WithApprovalContext(context.Background(), 111, 111), 111, "delegate this", nil)
	if err != nil {
		t.Fatalf("HandleStreamForChat: %v", err)
	}
	if !sawDone.Load() {
		t.Error("the spawned child never finished mid-turn (sweep timing not exercised)")
	}
	if stats == nil {
		t.Fatal("HandleStreamForChat stats = nil, want stats")
	}
	if stats.SubagentTokens != 250 || stats.Tokens != 260 {
		t.Errorf("TurnStats = %+v, want {Tokens:260 SubagentTokens:250} (10 main loop + 250 unfetched child)", stats)
	}
	if du := a.Budget.DayUsage(111); du.Subagent != 250 || du.Total != 260 {
		t.Errorf("budget DayUsage = %+v, want {Subagent:250 Total:260}", du)
	}
	// Exactly once: a second turn charges no subagent tokens for
	// the same task.
	_, stats2, err := a.HandleStreamForChat(permissions.WithApprovalContext(context.Background(), 111, 111), 111, "anything else?", nil)
	if err != nil {
		t.Fatalf("second HandleStreamForChat: %v", err)
	}
	if stats2.SubagentTokens != 0 || stats2.Tokens != 10 {
		t.Errorf("second TurnStats = %+v, want {Tokens:10 SubagentTokens:0} (the task was already swept)", stats2)
	}
}

// TestW3BUsageLinePricesPerModel proves F-A5-P2-18: /usage prices
// each (provider, model) slice of the day at that model's own rate.
// A morning on deepseek-chat must not be repriced at an afternoon
// switch to claude-opus-4.
func TestW3BUsageLinePricesPerModel(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Config.DefaultProvider = "deepseek"
	a.Budget = budget.New(1000000)
	// 100,000 deepseek-chat prompt tokens: 100,000 x $0.28/1M.
	if err := a.Budget.RecordUsageForModel(111, "deepseek", "deepseek-chat", model.Usage{PromptTokens: 100000, CompletionTokens: 0, TotalTokens: 100000}, 0); err != nil {
		t.Fatalf("RecordUsageForModel deepseek: %v", err)
	}
	// 1,000 claude-opus-4 prompt tokens: 1,000 x $15.00/1M.
	if err := a.Budget.RecordUsageForModel(111, "anthropic", "claude-opus-4", model.Usage{PromptTokens: 1000, CompletionTokens: 0, TotalTokens: 1000}, 0); err != nil {
		t.Fatalf("RecordUsageForModel opus: %v", err)
	}
	got := a.UsageLineForChat(111)
	// $0.0280 + $0.0150 = $0.0430, labelled as priced per model.
	if !strings.Contains(got, "Est. cost today (this chat): $0.0430 (est., priced per model,") {
		t.Errorf("/usage line = %q, want $0.0430 priced per model", got)
	}
	// The old misprice — the whole day (101,000 tokens) at the
	// current model's deepseek rate — would be $0.0283.
	if strings.Contains(got, "$0.0283") {
		t.Errorf("/usage line = %q, contains the whole-day-at-current-model misprice $0.0283", got)
	}
	if !strings.Contains(got, "Tokens today (this chat): 101,000 (101,000 prompt + 0 completion, incl. 0 subagent)") {
		t.Errorf("/usage line = %q, want the 101,000-token day total", got)
	}
}
