package core

// History-budget tests: trimHistoryToBudget keeps the system prompt
// and the latest user turn, preserves tool-call/tool-result pairing,
// truncates an oversized single result instead of failing, and the
// agent loop continues normally after a multi-megabyte tool result.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// totalTokens sums messageTokens over a request window.
func totalTokens(msgs []model.Message) int {
	n := 0
	for _, m := range msgs {
		n += messageTokens(m)
	}
	return n
}

// assertPairingIntact fails when a tool result has no assistant
// message immediately before it requesting that call ID, or an
// assistant tool call has no result following it.
func assertPairingIntact(t *testing.T, msgs []model.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role == "tool" {
			if i == 0 || msgs[i-1].Role != "assistant" {
				t.Fatalf("tool result at index %d (id %q) has no assistant message before it", i, m.ToolCallID)
			}
			found := false
			for _, tc := range msgs[i-1].ToolCalls {
				if tc.ID == m.ToolCallID {
					found = true
				}
			}
			if !found {
				t.Fatalf("tool result at index %d (id %q) was not requested by the preceding assistant message", i, m.ToolCallID)
			}
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				found := false
				for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
					if msgs[j].ToolCallID == tc.ID {
						found = true
					}
				}
				if !found {
					t.Fatalf("assistant tool call %q at index %d has no tool result following it", tc.ID, i)
				}
			}
		}
	}
}

func longHistory() []model.Message {
	msgs := []model.Message{{Role: "system", Content: systemPrompt}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			model.Message{Role: "user", Content: strings.Repeat("old user turn filler ", 100)}, // ~2000 chars
			model.Message{Role: "assistant", Content: "old answer", ToolCalls: []model.ToolCall{{ID: "call-old-" + strconv.Itoa(i), Name: "calculator", Arguments: `{"expression":"1+1"}`}}},
			model.Message{Role: "tool", Content: strings.Repeat("old tool result filler ", 100), ToolCallID: "call-old-" + strconv.Itoa(i)},
		)
	}
	msgs = append(msgs, model.Message{Role: "user", Content: "latest question: what is 2+2?"})
	return msgs
}

func TestTrimHistoryWithinBudgetUnchanged(t *testing.T) {
	msgs := []model.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	trimmed, stats := trimHistoryToBudget(msgs, 32768)
	if stats.dropped != 0 || stats.truncated != 0 {
		t.Fatalf("in-budget history was trimmed: %+v", stats)
	}
	if len(trimmed) != len(msgs) {
		t.Fatalf("in-budget history changed length: %d -> %d", len(msgs), len(trimmed))
	}
	// The result is a copy: mutating it must not touch the input.
	trimmed[1].Content = "mutated"
	if msgs[1].Content != "hi" {
		t.Fatal("trim returned the caller's slice, not a copy")
	}
}

func TestTrimHistoryKeepsSystemAndLatestUser(t *testing.T) {
	msgs := longHistory()
	trimmed, stats := trimHistoryToBudget(msgs, 2000)
	if stats.dropped == 0 {
		t.Fatal("expected old turns to be dropped under a 2000-token budget")
	}
	if totalTokens(trimmed) > 2000 {
		t.Fatalf("trimmed history still over budget: %d tokens", totalTokens(trimmed))
	}
	if trimmed[0].Role != "system" || trimmed[0].Content != systemPrompt {
		t.Fatal("system prompt was dropped or modified")
	}
	last := trimmed[len(trimmed)-1]
	if last.Role != "user" || last.Content != "latest question: what is 2+2?" {
		t.Fatalf("latest user turn did not survive: %+v", last)
	}
	assertPairingIntact(t, trimmed)
}

func TestTrimHistoryPreservesToolPairing(t *testing.T) {
	// A budget that forces dropping several whole turns: every
	// surviving tool result must still have its assistant call,
	// and every surviving call its result.
	for _, budget := range []int{3000, 1500, 1200} {
		trimmed, _ := trimHistoryToBudget(longHistory(), budget)
		if totalTokens(trimmed) > budget {
			t.Fatalf("budget %d: trimmed history %d tokens, over budget", budget, totalTokens(trimmed))
		}
		assertPairingIntact(t, trimmed)
		if trimmed[0].Content != systemPrompt {
			t.Fatalf("budget %d: system prompt modified", budget)
		}
	}
}

func TestTrimHistoryTruncatesOversizedSingleResult(t *testing.T) {
	// One tool result alone exceeds the budget: dropping turns
	// cannot help, so the result itself must be truncated with a
	// marker and the loop must still have a usable window.
	msgs := []model.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "read the file"},
		{Role: "assistant", Content: "", ToolCalls: []model.ToolCall{{ID: "call-1", Name: "file_read", Arguments: `{"path":"big.txt"}`}}},
		{Role: "tool", Content: strings.Repeat("z", 100*1024), ToolCallID: "call-1"},
	}
	trimmed, stats := trimHistoryToBudget(msgs, 2000)
	if stats.truncated == 0 {
		t.Fatal("expected the oversized tool result to be truncated")
	}
	if totalTokens(trimmed) > 2000 {
		t.Fatalf("trimmed history still over budget: %d tokens", totalTokens(trimmed))
	}
	if trimmed[0].Content != systemPrompt {
		t.Fatal("system prompt was modified")
	}
	toolMsg := trimmed[len(trimmed)-1]
	if toolMsg.Role != "tool" || !strings.Contains(toolMsg.Content, "[truncated for context budget:") {
		t.Fatalf("tool result missing the budget marker: len %d, tail %q", len(toolMsg.Content), toolMsg.Content[max(0, len(toolMsg.Content)-120):])
	}
	if !strings.Contains(toolMsg.Content, "total bytes") {
		t.Fatalf("budget marker should state the original size: tail %q", toolMsg.Content[max(0, len(toolMsg.Content)-120):])
	}
	assertPairingIntact(t, trimmed)
}

func TestTrimHistoryNeverDropsSystemWhenBudgetTiny(t *testing.T) {
	// Even a budget barely above the system prompt itself must
	// keep the system prompt and the latest user turn.
	msgs := longHistory()
	budget := messageTokens(model.Message{Role: "system", Content: systemPrompt}) + 200
	trimmed, _ := trimHistoryToBudget(msgs, budget)
	if trimmed[0].Content != systemPrompt {
		t.Fatal("system prompt was dropped or modified at a tiny budget")
	}
	if trimmed[len(trimmed)-1].Content != "latest question: what is 2+2?" {
		t.Fatal("latest user turn was dropped at a tiny budget")
	}
	assertPairingIntact(t, trimmed)
}

func TestNewAgentAppliesCapsToRegistry(t *testing.T) {
	reg := tools.NewRegistry()
	cfg := testConfig(5)
	cfg.MaxToolResultBytes = 4096
	cfg.MaxHistoryTokens = 8192
	a := newTestAgent(t, cfg, &fakeProvider{}, reg, newFakeStore())
	if got := reg.MaxResultBytes(); got != 4096 {
		t.Fatalf("registry cap = %d, want 4096 (from agent config)", got)
	}
	if a.maxHistoryTokens() != 8192 {
		t.Fatalf("agent history budget = %d, want 8192", a.maxHistoryTokens())
	}
	// Zero caps in config mean the defaults, not zero.
	cfg2 := testConfig(5)
	cfg2.MaxToolResultBytes = 0
	cfg2.MaxHistoryTokens = 0
	reg2 := tools.NewRegistry()
	newTestAgent(t, cfg2, &fakeProvider{}, reg2, newFakeStore())
	if got := reg2.MaxResultBytes(); got != config.DefaultMaxToolResultBytes {
		t.Fatalf("registry cap = %d, want default %d", got, config.DefaultMaxToolResultBytes)
	}
}

func TestRunContinuesAfterMultiMBToolResult(t *testing.T) {
	// End to end: a 3 MiB file is read, the result is capped by the
	// registry (256 KiB) and then truncated again to the history
	// budget (1500 tokens), and the loop still completes the turn
	// normally instead of failing.
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("w", 3*1024*1024)), 0o600); err != nil {
		t.Fatalf("write big file: %v", err)
	}
	mem := newFakeStore()
	for i := 0; i < 12; i++ {
		_ = mem.AddMessage(model.Message{Role: "user", Content: strings.Repeat("old filler turn ", 200)})
		_ = mem.AddMessage(model.Message{Role: "assistant", Content: strings.Repeat("old filler answer ", 200)})
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	args, _ := json.Marshal(map[string]any{"path": path})
	prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		// The seeded history is ~13x the budget, so the turn
		// starts with a bounded emergency compaction (Audit 5
		// F-A5-1): answer the summarisation call with a real
		// summary — it must preserve the seeded turns' tokens —
		// not with the file_read tool call.
		if len(req.Messages) > 0 && req.Messages[0].Content == "You summarise conversations into compact memories." {
			return textResponse("Summary of the old filler turns: old filler turn and old filler answer repeated."), nil
		}
		// The turn's own requests: the first asks to read the
		// file; any request already carrying the tool result is
		// done.
		for _, m := range req.Messages {
			if m.Role == "tool" {
				return textResponse("done"), nil
			}
		}
		return toolCallResponse("call-1", "file_read", string(args)), nil
	}}
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1500
	agent := newTestAgent(t, cfg, prov, reg, mem)
	out, err := agent.Run(context.Background(), "read the big file and summarise it")
	if err != nil {
		t.Fatalf("Run should continue normally after a huge tool result, got error: %v", err)
	}
	if out != "done" {
		t.Fatalf("Run = %q, want \"done\"", out)
	}
	// The provider's second call saw a window within budget, with
	// the tool result truncated for the budget, and pairing intact.
	req := prov.lastReq
	if totalTokens(req.Messages) > 1500 {
		t.Fatalf("provider request %d tokens, over the 1500-token budget", totalTokens(req.Messages))
	}
	if req.Messages[0].Content != systemPrompt {
		t.Fatal("system prompt was modified in the provider request")
	}
	var toolMsg *model.Message
	for i := range req.Messages {
		if req.Messages[i].Role == "tool" {
			toolMsg = &req.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("provider request has no tool result")
	}
	if !strings.Contains(toolMsg.Content, "[truncated for context budget:") {
		t.Fatalf("tool result in provider request was not truncated for the budget (len %d)", len(toolMsg.Content))
	}
	assertPairingIntact(t, req.Messages)
	// The loop stored the full (capped) tool result in memory even
	// though the provider only saw the trimmed window.
	stored := mem.allMessages()
	found := false
	for _, m := range stored {
		if m.Role == "tool" && strings.Contains(m.Content, "[truncated:") {
			found = true
		}
	}
	if !found {
		t.Fatal("memory should hold the registry-capped tool result")
	}
}

func TestRunTrimsRecallWithinBudget(t *testing.T) {
	// Recall injection + a tight budget: the recalled fact must
	// survive in the system message (it is never dropped) while
	// the oldest turns pay for it and the request stays in budget.
	mem := newFakeStore()
	_ = mem.AddFact("favourite number", "17")
	_ = mem.AddMessage(model.Message{Role: "user", Content: "My favourite number is 17."})
	for i := 0; i < 12; i++ {
		_ = mem.AddMessage(model.Message{Role: "user", Content: strings.Repeat("old filler turn ", 200)})
		_ = mem.AddMessage(model.Message{Role: "assistant", Content: strings.Repeat("old filler answer ", 200)})
	}
	prov := &fakeProvider{}
	reg := tools.NewRegistry()
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1500
	agent := newTestAgent(t, cfg, prov, reg, mem)
	out, err := agent.Run(context.Background(), "what is my favourite number times 3")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "canned answer" {
		t.Fatalf("Run = %q, want canned answer", out)
	}
	req := prov.lastReq
	if totalTokens(req.Messages) > 1500 {
		t.Fatalf("provider request %d tokens, over the 1500-token budget", totalTokens(req.Messages))
	}
	if !strings.Contains(req.Messages[0].Content, "17") {
		t.Fatal("recall fact did not survive trimming in the system message")
	}
	if req.Messages[len(req.Messages)-1].Content != "what is my favourite number times 3" {
		t.Fatal("latest user turn did not survive trimming")
	}
}
