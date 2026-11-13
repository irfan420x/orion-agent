package app

// rc9 (Worker E) tests: USD cost display, subagent token attribution
// to the footer stats, and per-chat budget enforcement on the
// per-chat turn path (the path `orion daemon` and oriond share).
import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/model"
)

// usageScriptProvider is a scripted model.Provider for cost tests:
// Chat answers per the scripted function and counts calls, so tests
// can prove a refused turn never reached the provider.
type usageScriptProvider struct {
	mu     sync.Mutex
	calls  int
	chatFn func(call int, req model.ChatRequest) (*model.ChatResponse, error)
}

func (p *usageScriptProvider) Name() string { return "stub" }

func (p *usageScriptProvider) Chat(_ context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	return p.chatFn(call, req)
}

func (p *usageScriptProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := fn(resp.Message.Content); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *usageScriptProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// chargeTokensTool is a test tool that reports subagent tokens to the
// chat's agent mid-turn, the way the subagent_result tool does when
// the parent consumes a finished subagent result.
type chargeTokensTool struct {
	agent  *core.Agent
	tokens int
}

func (t *chargeTokensTool) Name() string        { return "charge" }
func (t *chargeTokensTool) Description() string { return "Reports subagent tokens to the agent." }
func (t *chargeTokensTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "charge", Description: t.Description(), Parameters: map[string]any{"type": "object"}}
}
func (t *chargeTokensTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	t.agent.AddSubagentUsage(t.tokens)
	return "charged", nil
}

func finalUsageResponse(content string, u model.Usage) *model.ChatResponse {
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: content},
		FinishReason: "stop",
		Usage:        u,
	}
}

// TestHandleStreamForChatStatsCarrySubagentSplit proves follow-up
// (1): a turn that consumes a subagent result reports the split in
// its TurnStats — the footer total honestly includes the subagent
// tokens, and SubagentTokens breaks them out — and the budget
// records the same split.
func TestHandleStreamForChatStatsCarrySubagentSplit(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		if call == 1 {
			return &model.ChatResponse{
				Message: model.Message{
					Role:      "assistant",
					ToolCalls: []model.ToolCall{{ID: "c1", Name: "charge", Arguments: "{}"}},
				},
				FinishReason: "tool_calls",
			}, nil
		}
		return finalUsageResponse("done", model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}), nil
	}}
	a.Provider = prov
	agent, err := a.AgentForChat(111)
	if err != nil {
		t.Fatalf("AgentForChat: %v", err)
	}
	if err := a.Tools.Register(&chargeTokensTool{agent: agent, tokens: 42}); err != nil {
		t.Fatalf("Register charge tool: %v", err)
	}
	_, stats, err := a.HandleStreamForChat(context.Background(), 111, "do it", nil)
	if err != nil {
		t.Fatalf("HandleStreamForChat: %v", err)
	}
	if stats == nil {
		t.Fatal("HandleStreamForChat stats = nil, want stats")
	}
	if stats.Tokens != 57 || stats.PromptTokens != 10 || stats.CompletionTokens != 5 || stats.SubagentTokens != 42 {
		t.Errorf("TurnStats = %+v, want {Tokens:57 Prompt:10 Completion:5 Subagent:42}", stats)
	}
	du := a.Budget.DayUsage(111)
	if du.Prompt != 10 || du.Completion != 5 || du.Subagent != 42 || du.Total != 57 {
		t.Errorf("budget DayUsage = %+v, want {Prompt:10 Completion:5 Subagent:42 Total:57}", du)
	}
}

// TestHandleStreamForChatBudgetRefusal proves the budget gate on
// the per-chat turn path under the F-A5-6 projection semantics: a
// turn whose projected input cost exceeds the remaining budget is
// refused pre-flight, BEFORE the provider is called, and records
// nothing — the old pre-flight-only Check(chatID, 0) let such a
// turn run and blow past the limit. A turn that starts affordable
// but overspends mid-turn is aborted after the over-budget
// response (the spend that already happened is still recorded),
// and the next turn is refused pre-flight.
func TestHandleStreamForChatBudgetRefusal(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("ok", model.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}), nil
	}}
	a.Provider = prov
	// Chat 111 has 99/100 used: even the smallest turn's
	// projected input (~5 tokens for "first") no longer fits, so
	// the turn is refused pre-flight and never reaches the
	// provider.
	if err := a.Budget.RecordUsage(111, model.Usage{TotalTokens: 99}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, err := a.HandleStreamForChat(context.Background(), 111, "first", nil); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("HandleStreamForChat over projected budget err = %v, want ErrBudgetExceeded", err)
	}
	if got := prov.Calls(); got != 0 {
		t.Errorf("provider calls = %d, want 0 (the refused turn must never reach the provider)", got)
	}
	// A refused turn records nothing.
	if got := a.Budget.Usage(111); got != 99 {
		t.Errorf("budget Usage after refused turn = %d, want 99", got)
	}
	// Chat 112 starts at 0, so its turn runs — but the provider
	// reports 150 tokens for a 100-token budget, so the mid-turn
	// abort stops the turn after that response. The spend that
	// already happened is still recorded.
	if _, _, err := a.HandleStreamForChat(context.Background(), 112, "first", nil); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("overspending HandleStreamForChat err = %v, want ErrBudgetExceeded", err)
	}
	if got := a.Budget.Usage(112); got != 150 {
		t.Fatalf("budget Usage after overspending turn = %d, want 150 (over-budget spend is recorded)", got)
	}
	if _, _, err := a.HandleStreamForChat(context.Background(), 112, "second", nil); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("second HandleStreamForChat err = %v, want ErrBudgetExceeded", err)
	}
	if got := prov.Calls(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (only the overspending turn reached the provider)", got)
	}
	// A refused turn records nothing.
	if got := a.Budget.Usage(112); got != 150 {
		t.Errorf("budget Usage after refused turn = %d, want 150", got)
	}
}

// TestHandleForChatBudgetEnforced proves the non-streaming per-chat
// path (used by `orion daemon`) enforces the same budget under the
// F-A5-6 projection semantics: a chat whose remaining budget cannot
// cover the turn's projected input is refused pre-flight without a
// provider call, while other chats are unaffected.
func TestHandleForChatBudgetEnforced(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("ok", model.Usage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10}), nil
	}}
	a.Provider = prov
	// Chat 111 has 99/100 used: the next turn's projected input
	// does not fit, so it is refused pre-flight.
	if err := a.Budget.RecordUsage(111, model.Usage{TotalTokens: 99}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, err := a.HandleForChat(context.Background(), 111, "first"); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("first HandleForChat err = %v, want ErrBudgetExceeded", err)
	}
	if got := prov.Calls(); got != 0 {
		t.Errorf("provider calls = %d, want 0 (the refused turn must never reach the provider)", got)
	}
	if got := a.Budget.Usage(111); got != 99 {
		t.Errorf("budget Usage after refused turn = %d, want 99", got)
	}
	// Other chats are unaffected by this chat's budget.
	if _, err := a.HandleForChat(context.Background(), 222, "other chat"); err != nil {
		t.Fatalf("HandleForChat for another chat: %v, want it to run", err)
	}
	if got := prov.Calls(); got != 1 {
		t.Errorf("provider calls after other chat = %d, want 1", got)
	}
}

// TestUsageLineForChat proves the /usage per-chat block: tokens
// today with the split, the estimated USD cost at the chat's
// provider/model, and the daily budget with remaining + reset.
func TestUsageLineForChat(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Config.DefaultProvider = "deepseek"
	a.Budget = budget.New(100000)
	if err := a.Budget.RecordUsage(111, model.Usage{PromptTokens: 3000, CompletionTokens: 337, TotalTokens: 3337}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	got := a.UsageLineForChat(111)
	for _, want := range []string{
		"Tokens today (this chat): 3,337 (3,000 prompt + 337 completion, incl. 0 subagent)",
		"Est. cost today (this chat): $0.0010 (est., deepseek-chat, price table v2026-11-13 effective 2026-11-13)",
		"Daily token budget: 100,000 tokens (96,663 remaining, resets ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("UsageLineForChat =\n%s\nmissing %q", got, want)
		}
	}
}

// TestUsageLineForChatUnknownPrice proves an unpriced model reports
// "unknown" — never $0.00 for a paid model.
func TestUsageLineForChatUnknownPrice(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Config.DefaultProvider = "deepseek"
	entry := a.Config.Providers["deepseek"]
	entry.Model = "deepseek-v99"
	a.Config.Providers["deepseek"] = entry
	a.Budget = budget.New(100000)
	if err := a.Budget.RecordUsage(111, model.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	got := a.UsageLineForChat(111)
	if !strings.Contains(got, "Est. cost today (this chat): unknown (no price for deepseek/deepseek-v99 in price table v2026-11-13") {
		t.Errorf("UsageLineForChat =\n%s\nwant the unknown-price line, not a $0 cost", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("UsageLineForChat =\n%s\nmust not show $0.00 for an unpriced model", got)
	}
}

// TestUsageLineForChatUnlimitedAndNoBudget covers the two
// degenerate shapes: an unlimited budget, and a hand-built App
// with no budget at all.
func TestUsageLineForChatUnlimitedAndNoBudget(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Config.DefaultProvider = "ollama"
	a.Budget = budget.New(0) // unlimited
	if err := a.Budget.RecordUsage(111, model.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	got := a.UsageLineForChat(111)
	if !strings.Contains(got, "Daily token budget: unlimited") {
		t.Errorf("UsageLineForChat (unlimited) =\n%s\nwant 'Daily token budget: unlimited'", got)
	}
	if !strings.Contains(got, "Est. cost today (this chat): $0.0000 (est., llama3.1, price table v2026-11-13") {
		t.Errorf("UsageLineForChat (ollama) =\n%s\nwant the $0.0000 local-model estimate", got)
	}

	b := testApp(t)
	defer b.Close()
	if got := b.UsageLineForChat(111); !strings.Contains(got, "not tracked (no budget configured)") {
		t.Errorf("UsageLineForChat (nil Budget) = %q, want 'not tracked'", got)
	}
}
